// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"fmt"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/state"
)

// Orphan is a billing resource that local state does not fully account for.
type Orphan struct {
	Instance core.Instance
	Label    core.Label
	Known    bool // a rig snapshot exists locally
	Reason   string
}

// Describe renders an orphan for someone deciding whether to destroy it.
//
// The marker carries the answer even when local state does not (§5.3), which
// is the situation this command exists for: a lost disk, a different laptop, a
// process killed before it could clean up. A bare instance ID would leave the
// operator guessing whether it is theirs and what it was doing.
func (o Orphan) Describe() string {
	s := fmt.Sprintf("%s  %s  $%.4f/hr", o.Instance.InstanceID, o.Instance.Status, o.Instance.PriceHr)
	if !o.Instance.Running {
		// STOPPED is the trap: the GPU is released, the meter looks stopped,
		// and storage keeps billing until the resource is destroyed (§12.4).
		s += "  [not running — storage still bills]"
	}
	if d := o.Label.Describe(); d != "" {
		s += "\n      " + d
	}
	if o.Reason != "" {
		s += "\n      " + o.Reason
	}
	return s
}

// Orphans reports every LARRI-labelled resource at the provider, and whether
// local state knows about it.
//
// FR-DEL-05: the listing is provider-side, not state-side. Asking local state
// what exists would only ever return what LARRI already remembers, and the
// resources that matter are precisely the ones it does not.
func (o *Orchestrator) Orphans(ctx context.Context) ([]Orphan, error) {
	live, err := o.Provider.List(ctx)
	if err != nil {
		return nil, err
	}
	known := map[string]*core.Rig{}
	if rigs, lerr := o.Store.List(); lerr == nil {
		for _, r := range rigs {
			known[r.ID] = r
		}
	}
	var out []Orphan
	for _, inst := range live {
		rigID, ours := inst.RigID()
		if !ours {
			continue // the operator's own instance; not ours to touch
		}
		l, _ := inst.Label()
		if o.LabelSealer != nil {
			if raw, ok := inst.Labels[core.LabelRawKey]; ok {
				if opened, ok2 := core.DecodeLabelWith(raw, o.LabelSealer); ok2 {
					l = opened
				}
			}
		}
		orph := Orphan{Instance: inst, Label: l}
		rig, isKnown := known[rigID]
		orph.Known = isKnown
		switch {
		case !isKnown:
			orph.Reason = "local state has no record of this rig"
		case rig.State == core.StateDestroyed:
			orph.Reason = "local state says DESTROYED, but the resource is still here"
		case !rig.State.ExpectsInstance():
			orph.Reason = "local state says " + string(rig.State) +
				", which should have no resource behind it"
		default:
			// Tracked, and in a state where billing is exactly what should be
			// happening. That is the current rig, not an orphan.
			//
			// It was listed as one, with the reason "tracked as
			// BOOTSTRAPPING" printed directly beneath the heading "local
			// state does not account for" — a contradiction the output stated
			// on consecutive lines. The consequence was not cosmetic:
			// `larri orphans --destroy` offered to destroy a live bring-up
			// that had been running for half an hour, and answering yes would
			// have killed it.
			//
			// A tracked rig whose process has since died is a real situation
			// and a different one: `larri status` shows it, `larri resume`
			// reattaches to it, and `larri down` ends it. None of those need
			// it miscalled an orphan.
			continue
		}
		out = append(out, orph)
	}
	return out, nil
}

// DestroyOrphan removes one resource and proves it is gone.
//
// The proof is the point: a delete that returns success has made a claim, and
// a stopped container satisfies neither the claim nor the operator's bill
// (FR-DEL-03).
func (o *Orchestrator) DestroyOrphan(ctx context.Context, instanceID string) error {
	o.emit("orphans", "destroying %s", instanceID)
	if err := o.Provider.Destroy(ctx, instanceID); err != nil {
		o.warn("orphans", "destroy call failed: %v", err)
	}
	confirmed, err := o.confirmAbsent(ctx, instanceID)
	if err != nil {
		return err
	}
	if !confirmed {
		return errs.Newf(errs.ClassDestroyUnconfirmed, "daemon.DestroyOrphan",
			"%s not confirmed absent — check the provider dashboard", instanceID)
	}
	o.emit("orphans", "confirmed absent")
	return nil
}

// SweepOrphans destroys every LARRI-labelled resource, confirming each.
func (o *Orchestrator) SweepOrphans(ctx context.Context) (int, error) {
	orphans, err := o.Orphans(ctx)
	if err != nil {
		return 0, err
	}
	var destroyed int
	var firstErr error
	for _, orph := range orphans {
		if err := o.DestroyOrphan(ctx, orph.Instance.InstanceID); err != nil {
			o.warn("orphans", "%v", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		destroyed++
		if orph.Known {
			o.recordReaped(orph)
		}
	}
	return destroyed, firstErr
}

// recordReaped journals that a tracked rig's resource was destroyed by a
// sweep, so the rig's history explains how it actually ended.
func (o *Orchestrator) recordReaped(orph Orphan) {
	rig, err := o.Store.Load(orph.Label.RigID)
	if err != nil || rig == nil {
		return
	}
	entries, _ := o.Store.Entries()
	rig.End = &core.Termination{
		Actor: core.ActorOperator, Code: core.ReasonOrphanSweep,
		At:      time.Now().UTC(),
		Summary: "destroyed by orphan sweep",
		Evidence: map[string]string{
			"instance": orph.Instance.InstanceID,
			"reason":   orph.Reason,
		},
		Cost: state.CostFor(entries, rig.ID, time.Now().UTC()),
	}
	_ = o.Store.Transition(rig, core.StateDestroyed, "orphan sweep")
}
