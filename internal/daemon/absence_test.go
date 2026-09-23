// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/provider"
	pfake "go.sovrenix.com/larri/internal/provider/fake"
	rfake "go.sovrenix.com/larri/internal/runtime/fake"
	"go.sovrenix.com/larri/internal/state"
)

// legacyRig journals a create that failed and a teardown that recorded no
// reason — the shape of every record made before a failed create could settle
// itself — with an hour between each entry, so it has real cost.
func legacyRig(t *testing.T, st *state.Store, instance string) *core.Rig {
	t.Helper()
	hourly(st)
	id, err := state.NewID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rig := &core.Rig{ID: id, State: core.StateSelected, Runtime: core.RuntimeVLLM, Offer: offers()[0], Model: upReq().Model}
	for _, to := range []core.LifecycleState{core.StateCreating, core.StateFailed, core.StateDraining} {
		if err := st.Transition(rig, to, "legacy"); err != nil {
			t.Fatal(err)
		}
	}
	if instance != "" {
		rig.Instance = &core.Instance{Provider: "fake", InstanceID: instance}
		if err := st.Transition(rig, core.StateDraining, "legacy"); err != nil {
			t.Fatal(err)
		}
		rig.Instance = nil
	}
	rig.End = &core.Termination{Actor: core.ActorOperator, Code: core.ReasonOperatorRequest,
		Summary: "requested from the CLI"}
	if err := st.Transition(rig, core.StateDestroyed, "requested from the CLI"); err != nil {
		t.Fatal(err)
	}
	return rig
}

func costOf(t *testing.T, st *state.Store, rig *core.Rig) float64 {
	t.Helper()
	entries, _ := st.Entries()
	last := entries[len(entries)-1].At
	return state.CostFor(entries, rig.ID, last.Add(time.Hour)).TotalUSD
}

// The $111 record: a create refused for a missing key, destroyed days later
// by a teardown that did not say why. Only the operator can say nothing was
// created; once they have, it stops accruing and the record says who said so.
func TestAnOperatorCanRecordThatALegacyRigCreatedNothing(t *testing.T) {
	o, _, st := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	rig := legacyRig(t, st, "")
	if costOf(t, st, rig) == 0 {
		t.Fatal("the legacy record accrues nothing; the test proves nothing")
	}
	if err := o.RecordNothingCreated(context.Background(), rig, "runpod billing shows no charge"); err != nil {
		t.Fatal(err)
	}
	if c := costOf(t, st, rig); c != 0 {
		t.Errorf("still accrues $%.4f after the operator recorded nothing was created", c)
	}
	saved, _ := st.Load(rig.ID)
	if got := saved.End.Evidence[core.EvidenceNothingCreated]; !strings.HasPrefix(got, "operator: ") {
		t.Errorf("evidence = %q; a correction on someone's word must say whose", got)
	}
	if saved.End.Summary != "requested from the CLI" {
		t.Errorf("summary = %q; the original reason the rig ended is kept", saved.End.Summary)
	}
}

// Each refusal is a fact that proves the operator wrong, or a rig the
// ordinary teardown should handle.
func TestNothingCreatedIsRefusedWhereTheRecordSaysOtherwise(t *testing.T) {
	t.Run("an instance id was recorded", func(t *testing.T) {
		o, _, st := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
		rig := legacyRig(t, st, "fake-7")
		err := o.RecordNothingCreated(context.Background(), rig, "checked")
		if err == nil || !strings.Contains(err.Error(), "had instance fake-7") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("an instance carries its label now", func(t *testing.T) {
		o, p, st := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
		rig := legacyRig(t, st, "")
		if _, err := p.Create(context.Background(), offers()[0],
			provider.CreateSpec{Label: core.LabelKey + ":" + rig.ID}); err != nil {
			t.Fatal(err)
		}
		err := o.RecordNothingCreated(context.Background(), rig, "checked")
		if err == nil || !strings.Contains(err.Error(), "carries rig") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("the rig is not destroyed", func(t *testing.T) {
		o, _, st := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
		id, _ := state.NewID(time.Now())
		rig := &core.Rig{ID: id, State: core.StateSelected, Runtime: core.RuntimeVLLM, Offer: offers()[0]}
		if err := st.Transition(rig, core.StateFailed, "create failed"); err != nil {
			t.Fatal(err)
		}
		if err := o.RecordNothingCreated(context.Background(), rig, "checked"); err == nil {
			t.Error("recorded a live rig as never created; larri down checks that itself")
		}
	})
	t.Run("no note", func(t *testing.T) {
		o, _, st := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
		rig := legacyRig(t, st, "")
		if err := o.RecordNothingCreated(context.Background(), rig, "  "); err == nil {
			t.Error("recorded a correction without saying how it was checked")
		}
	})
}

// A different provider answers "not found" for an instance it never held,
// which is what confirmed absence looks like. Teardown through it would
// record a billing machine destroyed.
func TestARigIsNeverTornDownThroughAnotherProvider(t *testing.T) {
	o, p, st := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	rig, err := o.Up(context.Background(), upReq())
	if err != nil {
		t.Fatal(err)
	}
	other := pfake.New("elsewhere", nil, pfake.Behaviour{})
	wrong := &Orchestrator{Store: st, Provider: other, Runtime: rfake.New(rfake.Behaviour{})}

	err = wrong.Down(context.Background(), rig, nil)
	if err == nil || !strings.Contains(err.Error(), "is on fake, not elsewhere") {
		t.Fatalf("err = %v; teardown must refuse a provider that does not hold the rig", err)
	}
	if rig.State == core.StateDestroyed {
		t.Error("recorded DESTROYED through a provider that never held the instance")
	}
	if p.Count() != 1 {
		t.Error("the real instance changed")
	}
	if _, err := wrong.Adopt(context.Background(), rig.ID); err == nil {
		t.Error("adopted a rig through a provider that does not hold it")
	}
}

// A rig destroyed before endings were recorded has none. The correction is
// then the first record of why it ended, and it is dated.
func TestACorrectionToARigWithNoEndingIsDated(t *testing.T) {
	o, _, st := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	rig := legacyRig(t, st, "")
	rig.End = nil
	if err := st.Save(rig); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC().Add(-time.Second)
	if err := o.RecordNothingCreated(context.Background(), rig, "checked the billing page"); err != nil {
		t.Fatal(err)
	}
	saved, _ := st.Load(rig.ID)
	if saved.End == nil || saved.End.At.Before(before) {
		t.Errorf("ending = %+v; the correction is dated when it was made", saved.End)
	}
}

// An orphan id sent to a provider that never held it must not come back
// destroyed: that provider's "not found" is exactly what confirmed absence
// looks like, and the instance goes on billing where it is.
func TestAnOrphanIsNeverDestroyedThroughAnotherProvider(t *testing.T) {
	o, p, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	inst, err := p.Create(context.Background(), offers()[0], provider.CreateSpec{Label: core.LabelKey + ":lost"})
	if err != nil {
		t.Fatal(err)
	}
	other := pfake.New("elsewhere", nil, pfake.Behaviour{})
	wrong := &Orchestrator{Store: o.Store, Provider: other, Runtime: o.Runtime}
	err = wrong.DestroyOrphan(context.Background(), inst.InstanceID)
	if err == nil || !strings.Contains(err.Error(), "is not at elsewhere") {
		t.Fatalf("err = %v; an instance this provider does not hold was reported destroyed", err)
	}
	for _, c := range other.Calls {
		if c == "Destroy" {
			t.Error("issued a destroy for an instance the provider does not hold")
		}
	}
	if p.Count() != 1 {
		t.Error("the real instance changed")
	}
}
