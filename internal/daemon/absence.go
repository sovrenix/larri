// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"strings"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/state"
)

// RecordNothingCreated corrects a destroyed rig's record on the operator's
// word that no instance ever existed for it, which stops its cost.
//
// It exists for records made before a failed create could settle itself. A
// create refused for a missing API key on 2026-09-08 created nothing, and was
// destroyed four days later by a `larri down` that did not yet record why; the
// journal kept it billing at $1.39/hr, and `larri status --all` still reports
// $111 for a pod that never existed.
//
// LARRI cannot establish that itself. A listing today shows what exists now,
// not what existed then, so absence from it proves nothing about the past — the
// operator has to have checked, at the provider, and the note says how. What
// LARRI can check it does, and each check is one that would prove the operator
// wrong: an instance id on record, or an instance carrying the rig's label
// right now, means something was created.
//
// Not exposed as a tool. It records what a person verified outside LARRI,
// and an agent calling it would be recording a verification nobody made.
func (o *Orchestrator) RecordNothingCreated(ctx context.Context, rig *core.Rig, how string) error {
	how = strings.TrimSpace(how)
	if how == "" {
		return errs.Newf(errs.ClassModelFailure, "daemon.RecordNothingCreated",
			"rig %s: say how you checked", rig.ID)
	}
	if err := o.holdsRig(rig, "daemon.RecordNothingCreated"); err != nil {
		return err
	}
	if rig.State != core.StateDestroyed {
		return errs.Newf(errs.ClassModelFailure, "daemon.RecordNothingCreated",
			"rig %s is %s: larri down ends it and checks the provider itself", rig.ID, rig.State)
	}
	if rig.End != nil && rig.End.Evidence[core.EvidenceNothingCreated] != "" {
		return errs.Newf(errs.ClassModelFailure, "daemon.RecordNothingCreated",
			"rig %s is already recorded as never created", rig.ID)
	}
	entries, err := o.Store.Entries()
	if err != nil {
		return err
	}
	if rig.Instance != nil && rig.Instance.InstanceID != "" {
		return errs.Newf(errs.ClassModelFailure, "daemon.RecordNothingCreated",
			"rig %s had instance %s", rig.ID, rig.Instance.InstanceID)
	}
	for _, e := range state.EntriesFor(entries, rig.ID) {
		if e.Instance != "" {
			return errs.Newf(errs.ClassModelFailure, "daemon.RecordNothingCreated",
				"rig %s had instance %s", rig.ID, e.Instance)
		}
	}
	found, checked := o.instanceForRig(ctx, rig)
	if found != nil {
		return errs.Newf(errs.ClassModelFailure, "daemon.RecordNothingCreated",
			"instance %s carries rig %s's label: larri orphans", found.InstanceID, rig.ID)
	}
	if !checked {
		o.warn("record", "could not reach %s to look for the rig's label; recording on your word alone",
			rig.ProviderName())
	}

	end := core.Termination{Actor: core.ActorOperator, Code: core.ReasonOperatorRequest,
		Summary: "recorded by the operator as never created"}
	if rig.End != nil {
		end = *rig.End
	}
	evidence := map[string]string{}
	for k, v := range end.Evidence {
		evidence[k] = v
	}
	evidence[core.EvidenceNothingCreated] = "operator: " + how
	end.Evidence = evidence
	end.Cost = core.CostSummary{}
	rig.End = &end
	return o.Store.Transition(rig, core.StateDestroyed, "operator recorded that nothing was created")
}
