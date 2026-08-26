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
	"sort"
	"strconv"
	"strings"
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

	// Ack, when non-nil, marks a synchronisation point rather than a message
	// to show. A surface closes it once everything queued ahead of it has
	// been rendered. Call Show rather than reading this directly.
	Ack chan struct{}
}

// Show reports whether e carries something to render, and releases e if it
// does not. Every surface draining an event channel must call it for each
// event and skip the ones it rejects — that call is what unblocks a sync.
func (e Event) Show() bool {
	if e.Ack != nil {
		close(e.Ack)
		return false
	}
	return true
}

// syncGrace bounds how long the lifecycle will wait for a surface to catch
// up. A renderer that has stopped draining must not be able to stall a run
// that is holding a rented GPU.
const syncGrace = 2 * time.Second

// Sync blocks until every event emitted so far has been rendered.
//
// Progress is delivered through a channel that surfaces drain on their own
// goroutine, so an emit is queued rather than shown. That is the right trade
// for progress — a lifecycle billing by the second must never block on a slow
// terminal — but it is wrong immediately before a prompt. The question
// reaches the terminal ahead of the report it refers to, and the operator is
// asked to approve a purchase underneath output that is still arriving; worse,
// a prompt left without its newline gets overwritten by the next line and the
// run looks hung when it is waiting to be answered.
//
// The channel is FIFO, so an acknowledged marker proves everything queued
// ahead of it has been written.
func (o *Orchestrator) Sync(ctx context.Context) {
	if o.Events == nil {
		return
	}
	ack := make(chan struct{})
	timer := time.NewTimer(syncGrace)
	defer timer.Stop()
	select {
	case o.Events <- Event{Ack: ack}:
	case <-ctx.Done():
		return
	case <-timer.C:
		return
	}
	select {
	case <-ack:
	case <-ctx.Done():
	case <-timer.C:
	}
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
	// is doing. Zero means fifteen seconds.
	//
	// A live run earned an HTTP 429 polling this endpoint, so the interval is
	// deliberately unhurried: the operator learns far more from the host
	// activity probe, which costs the provider nothing.
	BootPollInterval time.Duration

	// DeadmanDeadline is how long the *host* waits, without hearing from
	// LARRI, before deciding it has been abandoned and stopping itself.
	//
	// Zero derives it from IdleTimeout; negative disables the watchdog
	// entirely, which is a choice an operator can make and should have to
	// make deliberately.
	DeadmanDeadline time.Duration

	// IdleTimeout is the local reclamation window, used only to derive the
	// host's deadline so the two cannot invert. The local supervisor is the
	// one that enforces it — this field does not.
	IdleTimeout time.Duration

	// AuthStallTimeout ends the wait for a host to accept the rig key once
	// the provider stops reporting anything new. Zero means three minutes.
	//
	// Separate from BootStallTimeout because it measures a different thing: by
	// this point sshd is answering, and what is outstanding is the provider's
	// start-up script installing the key — which on a slow host runs after an
	// image pull that is still in flight.
	AuthStallTimeout time.Duration

	// AuthCap bounds that wait even while status keeps changing. Zero means
	// fifteen minutes.
	AuthCap time.Duration

	// HostProbeInterval is how often the host itself is asked whether it is
	// doing anything. Zero means twenty seconds.
	HostProbeInterval time.Duration

	// EndpointStallLimit is how long an advertised SSH endpoint may refuse
	// connections **while the provider reports nothing new** before the host
	// is judged dead. Zero means three minutes.
	//
	// This is the signal that catches a contract reporting running while its
	// container never starts — the provider's status cannot see that and the
	// connection can.
	//
	// Ninety seconds is measured rather than guessed. Across eleven live
	// rentals every host that ever answered did so within one poll of the
	// endpoint appearing — under thirty seconds, without exception — and no
	// host that failed to answer in that window ever answered at all. Five of
	// the eleven never answered, which is a dead-on-arrival rate near half on
	// the cheap tier, and none of it was predicted by the provider's
	// reliability score: every failed machine scored 0.94 or better.
	//
	// That reasoning produced ninety seconds and held until llama.cpp and
	// Ollama went to live hardware — which meant, for the first time, cards
	// that cost two cents an hour. The eleven rentals behind it were all on
	// the faster tier. On a GTX 1060 the gap between an image finishing its
	// pull and sshd answering repeatedly exceeded ninety seconds, and hosts
	// that were working were discarded at the last moment before they came
	// up.
	//
	// So the number is now three minutes, and the trade is lopsided in its
	// favour: waiting the extra ninety seconds on a genuinely dead host costs
	// about $0.0005, while discarding a live one costs the whole image pull
	// again somewhere else. The clock is also progress-driven now (§12.2.1.1)
	// — it only runs while the provider reports nothing new — so a dead host
	// is silent from the start and still caught early.
	EndpointStallLimit time.Duration

	// ColdStartLimit is how long the runtime may produce NO output at all
	// before the host is judged dead. Short on purpose: before there is a
	// single log line there is nothing to be patient about. Zero means four
	// minutes.
	ColdStartLimit time.Duration

	// WarmStallLimit is how long a runtime that HAS produced output may go
	// quiet, with the hardware also idle, before it is judged stalled.
	// Generous on purpose: a weight download is legitimately slow. Zero means
	// twelve minutes.
	WarmStallLimit time.Duration

	// ReadyCap bounds the whole readiness wait. Zero means thirty minutes.
	ReadyCap time.Duration

	// ReadyPollInterval is how often readiness is retried. Zero means ten
	// seconds.
	ReadyPollInterval time.Duration

	// HostIdleLimit is how long a reachable host may show no CPU, disk or
	// network activity before LARRI says so. Zero means five minutes.
	HostIdleLimit time.Duration

	// LabelSealer encrypts the descriptive half of the provider-side label.
	// Nil writes it in the clear, which is attributable but readable by the
	// host and the provider.
	LabelSealer core.Sealer

	// LabelLimit is the provider's cap on marker length. Zero uses the
	// conservative default.
	LabelLimit int

	// MaxHostAttempts bounds how many machines a single `up` will try before
	// giving up. Zero means three.
	MaxHostAttempts int

	// lastKeys carries the ephemeral identity from Up to Serve. Not
	// persisted: FR-STATE-05 forbids private keys in state files.
	lastKeys *sshx.KeyPair

	// lastBootStatus is the provider's most recent account of what the host
	// was doing, kept so a failure can say how far it got.
	lastBootStatus string

	// excludedMachines holds hosts already tried and found unusable this run.
	//
	// Keyed by machine rather than by offer, because a marketplace lists
	// several offers per physical host: a live run fell back twice and landed
	// on the same box each time, since only the offer ID had changed.
	excludedMachines []string

	// failedModels counts host failures per GPU model this run.
	//
	// Machine-level exclusion is not enough on a marketplace that lists a
	// deep pool of identical boxes. A live run failed on three different
	// "Tesla V100 128GB $0.109/hr" hosts in a row, each a distinct machine
	// with good reliability, each abandoned at the same phase — and the
	// ranking, which is dominated by price, walked straight back into the
	// same pool every time because a fresh cheap box always outranks a
	// dearer working one.
	//
	// Two independent hosts of one model failing identically is evidence
	// about the model or the seller behind it, not about the boxes. So the
	// second failure retires the model for the rest of the run.
	failedModels map[string]int
}

// modelStrikes is how many host failures on one GPU model retire it for the
// rest of the run. One is a bad box; two is a pattern worth acting on.
const modelStrikes = 2

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
// Survey is the result of sizing a model and ranking the market for it,
// without spending anything.
//
// It exists so `offers` and `up` cannot disagree. A preview that ran its own
// search would eventually recommend an offer that `up` then rejects, and the
// operator would have no way to tell which one was lying.
type Survey struct {
	Plan      core.SizingPlan
	Selection rank.Result
	Offers    int
}

// survey sizes the model and ranks the market. It never spends.
func (o *Orchestrator) survey(ctx context.Context, req UpRequest) (*Survey, error) {
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
		if ok, why := reqs.SatisfiesCUDA(parseCUDA(of.CUDAVersion)); !ok {
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
	if retired := o.retiredModels(); len(retired) > 0 {
		before := len(offers)
		kept := withoutModels(offers, retired)
		// Retiring a model must not empty the market. If nothing else fits,
		// a suspect box is still better than no rig at all, and the operator
		// has already been told what happened.
		if len(kept) > 0 {
			offers = kept
			if dropped := before - len(offers); dropped > 0 {
				o.emit("fallback", "skipping %d offers on %s — repeated failures this run",
					dropped, strings.Join(retired, ", "))
			}
		}
	}
	sel := rank.Select(offers, req.Criteria, fits, o.Policy)
	if sel.Selected == nil {
		short := sizing.Analyse(sizing.Request{Spec: req.Model, Facts: facts}, offers)
		return nil, errs.Newf(errs.ClassCriteriaUnsatisfiable, "daemon.survey", "%s", short.String())
	}
	return &Survey{Plan: plan, Selection: sel, Offers: len(offers)}, nil
}

// Offers ranks the market for a model without spending (FR-CLI-04).
func (o *Orchestrator) Offers(ctx context.Context, req UpRequest) (*Survey, error) {
	return o.survey(ctx, req)
}

func (o *Orchestrator) Up(ctx context.Context, req UpRequest) (*core.Rig, error) {
	sv, err := o.survey(ctx, req)
	if err != nil {
		return nil, err
	}
	plan, sel := sv.Plan, sv.Selection
	chosen := sel.Selected.Offer
	o.reportExclusions(sel)
	o.emit("select", "%s %s %dGB $%.3f/hr (reliability %.2f)",
		chosen.Provider, chosen.GPUModel, chosen.VRAMTotalGB(), chosen.PriceHr, chosen.Reliability)

	// The report above is queued, not printed. Let it land before asking.
	o.Sync(ctx)
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
		Image:  o.Runtime.Image(req.Model, plan),
		DiskGB: req.DiskGB,
		// Everything a recovering LARRI would need if local state were gone:
		// what was being served, on what runtime, since when, at what price,
		// behind which local port. Sealed when a key is configured; the rig
		// ID stays readable either way, because attribution must not depend
		// on holding a key.
		Label:   core.EncodeLabel(core.LabelFor(rig), o.LabelLimit, o.LabelSealer),
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
		if rig == nil {
			// Nothing was selected, so there is no machine to blame and
			// nothing to fall back from.
			return nil, err
		}
		lastErr = err
		// Say WHY this machine failed, here, rather than only reporting the
		// last error once every attempt is exhausted. A fallback that
		// swallows its reasons turns three distinct failures into one
		// unexplained one, and the operator is paying for each.
		o.warn("attempt", "failed on %s %s: %v",
			rig.Offer.Provider, rig.Offer.GPUModel, shortErr(err))
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
			if m := strings.TrimSpace(rig.Offer.GPUModel); m != "" {
				if o.failedModels == nil {
					o.failedModels = map[string]int{}
				}
				o.failedModels[m]++
				if o.failedModels[m] == modelStrikes {
					o.warn("fallback", "%d %s hosts failed the same way — trying different hardware",
						modelStrikes, m)
				}
			}
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
	// The provider's last word on what the host was doing. Without it a
	// post-mortem cannot tell a stalled image pull from a host that never
	// scheduled, which are different problems with different answers.
	if s := o.lastBootStatus; s != "" {
		term.Evidence["last_host_status"] = s
	}
	if rig.Offer.GPUModel != "" {
		term.Evidence["gpu"] = rig.Offer.GPUModel
		term.Evidence["price_hr"] = fmt.Sprintf("%.4f", rig.Offer.PriceHr)
	}
	if rig.Instance != nil {
		term.Evidence["instance"] = rig.Instance.InstanceID
	}
	if err := o.Down(ctx, rig, term); err != nil {
		o.warn("cleanup", "TEARDOWN UNCONFIRMED: %v — check the provider dashboard", err)
	}
}

// parseCUDA reads a provider's CUDA version string. Unparseable means
// unreported, which Requirements treats as no evidence rather than as a
// failure.
func parseCUDA(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}

// machineKey identifies the physical host behind an offer, or "" when the
// provider cannot name one.
//
// The exclusion list exists to stop a fallback retrying a *host* that just
// failed. That only means something where offers name hosts. A catalogue
// provider sells a GPU type and picks the machine itself, so its "offer id" is
// a class of hardware — and excluding it after one bad pod would take every
// RTX 4090 out of the running for the rest of the attempt, when retrying is
// exactly right: the next pod lands somewhere else.
//
// So an unidentifiable host is not excluded at all. MaxHostAttempts still
// bounds the retrying; what it must not do is bound it by throwing away the
// hardware the operator asked for.
func machineKey(o core.Offer) string {
	if o.MachineID == "" {
		return ""
	}
	return o.Provider + ":m" + o.MachineID
}

func (o *Orchestrator) hostProbeInterval() time.Duration {
	if o.HostProbeInterval > 0 {
		return o.HostProbeInterval
	}
	return 20 * time.Second
}

func (o *Orchestrator) hostIdleLimit() time.Duration {
	if o.HostIdleLimit > 0 {
		return o.HostIdleLimit
	}
	return 5 * time.Minute
}

// withoutMachines drops every offer on a host already tried this run.
//
// Excluding by machine rather than by offer is the point: a marketplace lists
// several offers per physical box, so an offer-keyed exclusion lets a fallback
// land on exactly the host that just failed — which a live run did twice, on
// the same "GTX 1660 S $0.036/hr" each time.
// retiredModels lists the GPU models that have failed enough times this run
// to stop being offered.
func (o *Orchestrator) retiredModels() []string {
	var out []string
	for m, n := range o.failedModels {
		if n >= modelStrikes {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// withoutModels drops every offer on a GPU model that has been retired.
func withoutModels(offers []core.Offer, models []string) []core.Offer {
	skip := make(map[string]bool, len(models))
	for _, m := range models {
		skip[m] = true
	}
	out := offers[:0:0]
	for _, of := range offers {
		if !skip[strings.TrimSpace(of.GPUModel)] {
			out = append(out, of)
		}
	}
	return out
}

func withoutMachines(offers []core.Offer, machines []string) []core.Offer {
	skip := make(map[string]bool, len(machines))
	for _, m := range machines {
		if m != "" {
			skip[m] = true
		}
	}
	out := offers[:0:0]
	for _, of := range offers {
		k := machineKey(of)
		if k == "" || !skip[k] {
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
