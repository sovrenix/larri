// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
	pfake "go.sovrenix.com/larri/internal/provider/fake"
	"go.sovrenix.com/larri/internal/state"
)

func probingPolicy() SupervisePolicy {
	return SupervisePolicy{PollInterval: 5 * time.Millisecond, HealthInterval: 5 * time.Millisecond}
}

// The live run that found it: `larri down` in a second terminal destroyed the
// pod and recorded it, and the `larri up` still holding the rig wrote DEGRADED
// over DESTROYED three failed probes later. Status showed a serving rig, the
// journal costed a pod that no longer existed, and a new `larri up` would
// have been refused as a second billing rig.
func TestASupervisorNeverWritesOverARigEndedElsewhere(t *testing.T) {
	o, live := liveRig(t)
	stale, err := o.Store.Load(live.Rig.ID)
	if err != nil {
		t.Fatal(err)
	}
	other := &Orchestrator{Store: o.Store, Provider: o.Provider, Runtime: o.Runtime}
	if err := other.Down(context.Background(), stale, nil); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	term := o.Supervise(ctx, live, probingPolicy())
	if term == nil {
		t.Fatal("the supervisor kept holding a rig that had been destroyed")
	}
	if term.Actor != core.ActorOperator {
		t.Errorf("termination %s/%s; the operator ended it", term.Actor, term.Code)
	}
	saved, _ := o.Store.Load(live.Rig.ID)
	if saved.State != core.StateDestroyed {
		t.Errorf("stored state = %s; a destroyed rig stays destroyed", saved.State)
	}
	// And the teardown that follows does nothing a second time.
	if err := o.Down(context.Background(), live.Rig, term); err != nil {
		t.Fatal(err)
	}
	entries, _ := o.Store.Entries()
	ended := 0
	for _, e := range state.EntriesFor(entries, live.Rig.ID) {
		if e.To == core.StateDestroyed {
			ended++
		}
		if e.To == core.StateDegraded {
			t.Error("journalled DEGRADED after the rig was destroyed")
		}
	}
	if ended != 1 {
		t.Errorf("journalled %d endings, want 1", ended)
	}
}

// An instance destroyed outside LARRI — the provider's console, a sweep —
// stops answering, and only the provider can say it is gone.
func TestAnInstanceTheProviderNoLongerHasEndsSupervision(t *testing.T) {
	o, live := liveRig(t)
	o.Provider.(*pfake.Provider).Vanish(live.Rig.Instance.InstanceID)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	term := o.Supervise(ctx, live, probingPolicy())
	if term == nil || term.Code != core.ReasonInstanceGone || term.Actor != core.ActorProvider {
		t.Fatalf("termination = %+v, want provider/instance-gone", term)
	}
	if term.Evidence["instance"] != live.Rig.Instance.InstanceID {
		t.Errorf("evidence = %v", term.Evidence)
	}
}

// A provider that cannot be reached concludes nothing: failing probes and an
// unanswered query leave the rig held and DEGRADED, not ended.
func TestAnUnreachableProviderDoesNotEndARig(t *testing.T) {
	o, live := liveRig(t)
	o.Provider.(*pfake.Provider).SetUnreachable(true)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if term := o.Supervise(ctx, live, probingPolicy()); term != nil {
		t.Errorf("ended a rig on an unanswered query: %+v", term)
	}
}

// Two teardowns of one rig — an idle policy firing in `larri up` while the
// operator runs `larri down` — each hold a copy read before the other ended
// it. The first record stands and the second adopts it, whether the second
// notices at the start or only when it goes to write.
func TestTwoTeardownsRecordOneEnding(t *testing.T) {
	o, live := liveRig(t)
	first, _ := o.Store.Load(live.Rig.ID)
	second, _ := o.Store.Load(live.Rig.ID)
	late, _ := o.Store.Load(live.Rig.ID)

	idle := &core.Termination{Actor: core.ActorPolicy, Code: core.ReasonIdleTimeout, Summary: "idle"}
	if err := o.Down(context.Background(), first, idle); err != nil {
		t.Fatal(err)
	}
	if err := o.Down(context.Background(), second, nil); err != nil {
		t.Fatalf("a second teardown of an ended rig failed: %v", err)
	}
	if second.End == nil || second.End.Code != core.ReasonIdleTimeout {
		t.Errorf("second teardown holds %+v; it adopts the first record", second.End)
	}
	// One that passed the start check before the first finished meets the
	// store's refusal at the write, and adopts the record there.
	late.State = core.StateDraining
	late.End = &core.Termination{Actor: core.ActorOperator, Code: core.ReasonOperatorRequest}
	if err := o.recordEnd(late, "late"); err != nil {
		t.Fatalf("late write: %v", err)
	}
	saved, _ := o.Store.Load(live.Rig.ID)
	if saved.End.Code != core.ReasonIdleTimeout {
		t.Errorf("stored ending = %s; the first teardown's reason was replaced", saved.End.Code)
	}
	entries, _ := o.Store.Entries()
	ended := 0
	for _, e := range state.EntriesFor(entries, live.Rig.ID) {
		if e.To == core.StateDestroyed {
			ended++
		}
	}
	if ended != 1 {
		t.Errorf("journalled %d endings, want 1", ended)
	}
}
