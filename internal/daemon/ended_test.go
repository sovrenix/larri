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
