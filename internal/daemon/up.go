// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package daemon composes the layers into a rig lifecycle.
//
// Everything below it — provider, runtime, sizing, rank, state, wire, sshx —
// knows nothing about this package (§3). It is the only component that mutates
// state, and the only one that decides what happens next.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/notice"
	"go.sovrenix.com/larri/internal/provider"
	"go.sovrenix.com/larri/internal/rank"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/sizing"
	"go.sovrenix.com/larri/internal/sshx"
	"go.sovrenix.com/larri/internal/state"
	"go.sovrenix.com/larri/internal/wire"
)

// Event is a progress report from a lifecycle operation.
type Event struct {
	Phase   string
	Message string
	Warning bool
}

// Orchestrator runs rig lifecycles.
type Orchestrator struct {
	Store    *state.Store
	Provider provider.Provider
	Runtime  runtime.Runtime
	Resolver sizing.Resolver
	Policy   rank.Policy
	Proxy    *wire.Proxy

	// Deadline bounds the whole provisioning sequence. On expiry the rig is
	// torn down rather than abandoned (FR-PROV-04).
	Deadline time.Duration

	Events chan<- Event

	// BootStallTimeout ends a wait when the provider's status has stopped
	// changing. It is the real signal that a host gave up, where a fixed
	// deadline only measures how long an image happens to be. Zero means
	// eight minutes.
	BootStallTimeout time.Duration

	// BootCap bounds a boot even when the status keeps changing. Zero means
	// thirty minutes.
	BootCap time.Duration

	// BootPollInterval is how often the provider is asked what a booting host
	// is doing. Zero means ten seconds — often enough to render progress,
	// rare enough not to matter against a rate limit.
	BootPollInterval time.Duration

	// MaxHostAttempts bounds how many machines a single `up` will try before
	// giving up. Zero means three.
	MaxHostAttempts int

	// lastKeys carries the ephemeral identity from Up to Serve. Not
	// persisted: FR-STATE-05 forbids private keys in state files.
	lastKeys *sshx.KeyPair

	// excludedMachines holds hosts already tried and found unusable this run.
	//
	// Keyed by machine rather than by offer, because a marketplace lists
	// several offers per physical host: a live run fell back twice and landed
	// on the same box each time, since only the offer ID had changed.
	excludedMachines []string
}

func (o *Orchestrator) emit(phase, format string, args ...any) {
	if o.Events == nil {
		return
	}
	select {
	case o.Events <- Event{Phase: phase, Message: fmt.Sprintf(format, args...)}:
	default:
	}
}

func (o *Orchestrator) warn(phase, format string, args ...any) {
	if o.Events == nil {
		return
	}
	select {
	case o.Events <- Event{Phase: phase, Message: fmt.Sprintf(format, args...), Warning: true}:
	default:
	}
}

// UpRequest is what to provision.
type UpRequest struct {
	Criteria core.Criteria
	Model    core.ModelSpec
	DiskGB   int
	HFToken  secret.Secret
	Confirm  func(offer core.Offer, plan core.SizingPlan) bool

	// LocalPort is the fixed loopback port clients are wired against. Zero
	// lets the kernel choose, which is only useful in tests: P3 depends on
	// this being stable across the rig's life.
	LocalPort int
}

// Up provisions a rig and returns it ready to serve.
//
// The ordering is the design's, and the two lines that matter are the intent
// write before the create call, and the readiness check through the tunnel
// rather than on the host.
func (o *Orchestrator) Up(ctx context.Context, req UpRequest) (*core.Rig, error) {
	// ---- size before spending -------------------------------------------
	o.emit("sizing", "resolving %s", req.Model.Ref)
	facts, err := o.Resolver.Resolve(ctx, req.Model.Ref, req.Model.Revision)
	if err != nil {
		return nil, err
	}
	plan, err := sizing.Plan(sizing.Request{Spec: req.Model, Facts: facts})
	if err != nil {
		return nil, err
	}
	for _, w := range plan.Warnings {
		o.warn("sizing", "%s", w)
	}
	o.emit("sizing", "%s needs ~%s VRAM",
		req.Model.Ref, sizing.HumanBytes(plan.RequiredVRAMBytes))

	// ---- search and select ----------------------------------------------
	o.emit("search", "querying %s", o.Provider.Name())
	offers, err := o.Provider.Search(ctx, req.Criteria)
	if err != nil {
		return nil, err
	}
	o.emit("search", "%d offers satisfy the criteria", len(offers))

	// Fit is two questions, not one. VRAM answers "does the model hold"; the
	// runtime's requirements answer "can this hardware run the engine at
	// all". A live run selected a GTX 1060 because it passed the first and
	// nobody asked the second, and Pascal cannot serve with vLLM at any
	// price.
	reqs := o.Runtime.Requires()
	fits := func(of core.Offer) (bool, string) {
		if ok, why := reqs.Satisfies(of.ComputeCapability); !ok {
			return false, why
		}
		avail := uint64(of.VRAMTotalGB()) * sizing.GiB
		if avail >= plan.RequiredVRAMBytes {
			return true, ""
		}
		return false, fmt.Sprintf("%s short",
			sizing.HumanBytes(plan.RequiredVRAMBytes-avail))
	}
	if len(o.excludedMachines) > 0 {
		before := len(offers)
		offers = withoutMachines(offers, o.excludedMachines)
		if dropped := before - len(offers); dropped > 0 {
			o.emit("fallback", "skipping %d offers on %d host(s) already tried",
				dropped, len(o.excludedMachines))
		}
	}
	sel := rank.Select(offers, req.Criteria, fits, o.Policy)
	if sel.Selected == nil {
		short := sizing.Analyse(sizing.Request{Spec: req.Model, Facts: facts}, offers)
		return nil, errs.Newf(errs.ClassCriteriaUnsatisfiable, "daemon.Up", "%s", short.String())
	}
	chosen := sel.Selected.Offer
	o.reportExclusions(sel)
	o.emit("select", "%s %s %dGB $%.3f/hr (reliability %.2f)",
		chosen.Provider, chosen.GPUModel, chosen.VRAMTotalGB(), chosen.PriceHr, chosen.Reliability)

	if req.Confirm != nil && !req.Confirm(chosen, plan) {
		return nil, errs.Newf(errs.ClassModelFailure, "daemon.Up", "cancelled before spending")
	}

	// ---- mint the ID, then write intent, then spend ----------------------
	id, err := state.NewID(time.Now())
	if err != nil {
		return nil, err
	}
	rig := &core.Rig{
		ID: id, State: core.StateSelected, Criteria: req.Criteria,
		Model: req.Model, Runtime: o.Runtime.Kind(), Offer: chosen,
		Plan: plan, CreatedAt: time.Now().UTC(),
	}
	if err := o.Store.Save(rig); err != nil {
		return nil, err
	}
	keys, err := sshx.NewKeyPair()
	if err != nil {
		return nil, err
	}
	// FR-PROV-01. Everything after this line may fail, time out, or kill the
	// process; the journal already names the rig, provider, and offer, so
	// reconciliation can find whatever was created by its label.
	if err := o.Store.RecordIntent(rig, core.StateCreating, "create intent"); err != nil {
		return nil, err
	}
	rig.State = core.StateCreating

	o.emit("create", "renting %s at $%.3f/hr", chosen.OfferID, chosen.PriceHr)
	inst, err := o.Provider.Create(ctx, chosen, provider.CreateSpec{
		Image:   o.Runtime.Image(req.Model, plan),
		DiskGB:  req.DiskGB,
		Label:   core.LabelKey + ":" + rig.ID,
		OnStart: keys.OnStartScript(),
		// FR-SEC-15: SSH only. A container port that was never mapped is
		// unreachable regardless of what listens on it.
		Ports: nil,
	})
	if err != nil {
		_ = o.Store.RecordIntent(rig, core.StateFailed, "create failed: "+err.Error())
		return rig, err
	}
	rig.Instance = inst
	if err := o.Store.Transition(rig, core.StateProvisioned, "instance "+inst.InstanceID); err != nil {
		return rig, err
	}
	o.emit("create", "instance %s", inst.InstanceID)
	o.lastKeys = keys
	return rig, nil
}

// UpAndServe provisions a rig and brings it all the way to serving, falling
// back to the next-ranked offer when a host proves unusable.
//
// FR-PROV-05, and a live run showed why it is not optional. The cheapest
// eligible machine — reliability 0.98 — never accepted a connection at all.
// Without fallback that is fifteen minutes and a total failure; with it, it is
// a warning and the next offer. The distinction that governs it is the error
// class: a host failure means try elsewhere, while a model or config failure
// means the next host fails identically and retrying only spends more.
func (o *Orchestrator) UpAndServe(ctx context.Context, req UpRequest) (*Live, error) {
	attempts := o.MaxHostAttempts
	if attempts <= 0 {
		attempts = 3
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			o.warn("fallback", "attempt %d of %d on the next-ranked offer", attempt, attempts)
		}
		live, rig, err := o.attempt(ctx, req)
		if err == nil {
			return live, nil
		}
		lastErr = err
		// A deadline that expired while waiting on a host is a statement
		// about that host, so it earns a fallback like any other host
		// failure. A cancelled parent context is not: the operator asked to
		// stop.
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			err = errs.Newf(errs.ClassHostFailure, "daemon.attempt",
				"host did not finish coming up within the deadline")
			lastErr = err
		}
		if rig != nil && rig.Instance != nil {
			o.warn("cleanup", "tearing down rather than leaving it billing")
			o.teardownAfterFailure(rig, core.ReasonHostFailure, err)
			o.excludedMachines = append(o.excludedMachines, machineKey(rig.Offer))
		}
		// Only host-attributable failures are worth another machine.
		if errs.ClassOf(err) != errs.ClassHostFailure {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// attempt runs one full provisioning cycle against the best remaining offer.
//
// FR-PROV-04 requires the WHOLE sequence under one deadline, and an earlier
// version got this wrong in a way that only a hanging test revealed: Up owned
// the timeout and cancelled it on return, so everything after the create —
// waiting for sshd, pinning, bootstrap, launch, readiness — ran unbounded. A
// host that never finished booting would have held the attempt open forever
// while billing, which is the failure the deadline exists to prevent.
func (o *Orchestrator) attempt(ctx context.Context, req UpRequest) (*Live, *core.Rig, error) {
	deadline := o.Deadline
	if deadline == 0 {
		// Generous on purpose: a stock vLLM image is 10-15 GB and the weight
		// download follows it. The thing that ends a bad attempt early is
		// stall detection, not this ceiling.
		deadline = 45 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	rig, err := o.Up(ctx, req)
	if err != nil {
		return nil, rig, err
	}
	live, serr := o.Serve(ctx, rig, o.lastKeys, req.LocalPort, req.HFToken)
	if serr != nil {
		if live != nil {
			_ = live.Close()
		}
		return nil, rig, serr
	}
	return live, rig, nil
}

// teardownAfterFailure destroys a rig whose bring-up failed, on a fresh
// context so that a cancelled or expired parent cannot prevent the cleanup
// that stops the billing.
func (o *Orchestrator) teardownAfterFailure(rig *core.Rig, code core.ReasonCode, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	term := &core.Termination{
		Actor: core.ActorFault, Code: code, At: time.Now().UTC(),
		Summary:  "bring-up failed: " + shortErr(cause),
		Evidence: map[string]string{"error": shortErr(cause)},
	}
	if err := o.Down(ctx, rig, term); err != nil {
		o.warn("cleanup", "TEARDOWN UNCONFIRMED: %v — check the provider dashboard", err)
	}
}

// machineKey identifies the physical host behind an offer, falling back to the
// offer itself when the provider does not report one.
func machineKey(o core.Offer) string {
	if o.MachineID != "" {
		return o.Provider + ":m" + o.MachineID
	}
	return o.Provider + ":o" + o.OfferID
}

// withoutMachines drops every offer on a host already tried this run.
//
// Excluding by machine rather than by offer is the point: a marketplace lists
// several offers per physical box, so an offer-keyed exclusion lets a fallback
// land on exactly the host that just failed — which a live run did twice, on
// the same "GTX 1660 S $0.036/hr" each time.
func withoutMachines(offers []core.Offer, machines []string) []core.Offer {
	skip := make(map[string]bool, len(machines))
	for _, m := range machines {
		skip[m] = true
	}
	out := offers[:0:0]
	for _, of := range offers {
		if !skip[machineKey(of)] {
			out = append(out, of)
		}
	}
	return out
}

// reportExclusions summarises why offers were rejected.
//
// A live run printed fifty consecutive "above the ceiling" lines, which is
// noise rather than explanation: the operator needs to know each REASON that
// applied and roughly how much it removed, not to read every instance of the
// most common one. So the reasons are grouped, with a couple of examples each.
func (o *Orchestrator) reportExclusions(sel rank.Result) {
	type group struct {
		count    int
		examples []string
	}
	groups := map[rank.Reason]*group{}
	var order []rank.Reason
	for _, ex := range sel.Excluded() {
		if ex.Reason == rank.ReasonCostlier {
			continue
		}
		g, ok := groups[ex.Reason]
		if !ok {
			g = &group{}
			groups[ex.Reason] = g
			order = append(order, ex.Reason)
		}
		g.count++
		if len(g.examples) < 2 {
			g.examples = append(g.examples, fmt.Sprintf("%s %dGB $%.3f/hr — %s",
				ex.Offer.GPUModel, ex.Offer.VRAMTotalGB(), ex.Offer.PriceHr, ex.Detail))
		}
	}
	for _, reason := range order {
		g := groups[reason]
		o.emit("excluded", "%d offers: %s", g.count, reason)
		for _, e := range g.examples {
			o.emit("excluded", "    %s", e)
		}
		if g.count > len(g.examples) {
			o.emit("excluded", "    ... and %d more", g.count-len(g.examples))
		}
	}
}

// Down tears a rig down and confirms absence.
//
// Order matters: wiring is reverted before the instance is destroyed, so there
// is no window in which a client points at a dead endpoint. And the reason is
// resolved first, because a supervisor that destroys and reconstructs the
// motive afterwards gets it wrong exactly when several conditions were true at
// once (§13.1).
func (o *Orchestrator) Down(ctx context.Context, rig *core.Rig, term *core.Termination) error {
	if term == nil {
		term = &core.Termination{
			Actor: core.ActorOperator, Code: core.ReasonOperatorRequest,
			At: time.Now().UTC(), Summary: "requested from the CLI",
		}
	}
	if err := o.Store.RecordIntent(rig, core.StateDraining, term.Summary); err != nil {
		return err
	}
	rig.State = core.StateDraining

	if o.Proxy != nil {
		o.Proxy.SetUpstream(wire.Upstream{})
	}
	if rig.Instance == nil {
		rig.End = term
		return o.Store.Transition(rig, core.StateDestroyed, "no instance was ever created")
	}

	o.emit("destroy", "destroying %s", rig.Instance.InstanceID)
	if err := o.Provider.Destroy(ctx, rig.Instance.InstanceID); err != nil {
		o.warn("destroy", "destroy call failed: %v", err)
	}

	// A 200 from a delete endpoint is a claim. Absence from the inventory is
	// the evidence, and stopped is not absent (§12.4).
	confirmed, err := o.confirmAbsent(ctx, rig.Instance.InstanceID)
	if err != nil {
		return err
	}
	if !confirmed {
		o.warn("destroy", "UNCONFIRMED: %s may still be billing — check the provider dashboard",
			rig.Instance.InstanceID)
		return errs.Newf(errs.ClassDestroyUnconfirmed, "daemon.Down",
			"instance %s not confirmed absent", rig.Instance.InstanceID)
	}
	entries, _ := o.Store.Entries()
	term.Cost = state.CostFor(entries, rig.ID, time.Now().UTC())
	rig.End = term
	o.emit("destroy", "confirmed absent")
	return o.Store.Transition(rig, core.StateDestroyed, term.Summary)
}

func (o *Orchestrator) confirmAbsent(ctx context.Context, instanceID string) (bool, error) {
	backoff := time.Second
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		inst, err := o.Provider.Get(ctx, instanceID)
		if err == nil && inst == nil {
			return true, nil
		}
		if err != nil {
			// Unreachable is not absent (FR-SUP-11). Keep asking.
			o.warn("destroy", "verification query failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
	return false, nil
}

// PrivacyNotice is the standing disclosure, surfaced wherever a rig becomes
// usable.
func PrivacyNotice(rig *core.Rig) string {
	if rig.Instance == nil {
		return notice.PrivacyShort()
	}
	return notice.HostSummary(rig.Instance.Provider, rig.Instance.InstanceID, rig.Offer.Region)
}
