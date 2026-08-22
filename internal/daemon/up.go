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
}

// Up provisions a rig and returns it ready to serve.
//
// The ordering is the design's, and the two lines that matter are the intent
// write before the create call, and the readiness check through the tunnel
// rather than on the host.
func (o *Orchestrator) Up(ctx context.Context, req UpRequest) (*core.Rig, error) {
	deadline := o.Deadline
	if deadline == 0 {
		deadline = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

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

	fits := func(of core.Offer) (bool, string) {
		avail := uint64(of.VRAMTotalGB()) * sizing.GiB
		if avail >= plan.RequiredVRAMBytes {
			return true, ""
		}
		return false, fmt.Sprintf("%s short",
			sizing.HumanBytes(plan.RequiredVRAMBytes-avail))
	}
	sel := rank.Select(offers, req.Criteria, fits, o.Policy)
	if sel.Selected == nil {
		short := sizing.Analyse(sizing.Request{Spec: req.Model, Facts: facts}, offers)
		return nil, errs.Newf(errs.ClassCriteriaUnsatisfiable, "daemon.Up", "%s", short.String())
	}
	chosen := sel.Selected.Offer
	for _, ex := range sel.Excluded() {
		if ex.Reason != rank.ReasonCostlier {
			o.emit("excluded", "%s %dGB $%.3f/hr — %s",
				ex.Offer.GPUModel, ex.Offer.VRAMTotalGB(), ex.Offer.PriceHr, ex.Detail)
		}
	}
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
	return rig, nil
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
