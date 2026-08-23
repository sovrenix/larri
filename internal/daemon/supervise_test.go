// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/config"
	"go.sovrenix.com/larri/internal/core"
	pfake "go.sovrenix.com/larri/internal/provider/fake"
	rfake "go.sovrenix.com/larri/internal/runtime/fake"
	"go.sovrenix.com/larri/internal/wire"
)

// liveRig builds a serving rig with a real proxy, so the idle clock under test
// is the one the data plane actually feeds.
func liveRig(t *testing.T) (*Orchestrator, *Live) {
	t.Helper()
	o, _, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	rig, err := o.Up(context.Background(), upReq())
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := wire.NewProxy(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { proxy.Close() })
	return o, &Live{Rig: rig, proxy: proxy}
}

func fastPolicy(p SupervisePolicy) SupervisePolicy {
	p.PollInterval = 5 * time.Millisecond
	p.HealthInterval = time.Hour // health is not what these tests are about
	return p
}

// FR-SUP-06. The rig this product exists to prevent is the one that outlives
// the operator's attention, so an idle rig must actually die rather than be
// noted as idle.
func TestIdleTimeoutDestroys(t *testing.T) {
	o, live := liveRig(t)
	live.proxy.Activity.MarkOperator(time.Now().Add(-time.Hour))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	term := o.Supervise(ctx, live, fastPolicy(SupervisePolicy{
		Idle: config.Idle{Timeout: time.Minute, Action: config.IdleDestroy},
	}))
	if term == nil {
		t.Fatal("an hour-idle rig was left billing")
	}
	if term.Code != core.ReasonIdleTimeout {
		t.Errorf("code = %s, want idle-timeout", term.Code)
	}
	if term.Actor != core.ActorPolicy {
		t.Errorf("actor = %s, want policy", term.Actor)
	}
	// FR-DEL-08: a reason without evidence is not an explanation.
	for _, k := range []string{"idle_for", "window", "last_request", "probes_total"} {
		if term.Evidence[k] == "" {
			t.Errorf("evidence missing %q", k)
		}
	}
}

// warn mode must not destroy. An operator who asked to be told is not an
// operator who asked to lose the rig.
func TestIdleActionWarnDoesNotDestroy(t *testing.T) {
	o, live := liveRig(t)
	live.proxy.Activity.MarkOperator(time.Now().Add(-time.Hour))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if term := o.Supervise(ctx, live, fastPolicy(SupervisePolicy{
		Idle: config.Idle{Timeout: time.Minute, Action: config.IdleWarn},
	})); term != nil {
		t.Fatalf("warn mode destroyed the rig: %s", term.Summary)
	}
}

// A busy rig is not an idle rig, however long since the last request finished
// — a single long generation is work in progress.
func TestInFlightRequestIsNotIdle(t *testing.T) {
	o, live := liveRig(t)
	live.proxy.Activity.MarkOperator(time.Now().Add(-time.Hour))
	live.proxy.Activity.EnterInFlight()
	defer live.proxy.Activity.ExitInFlight()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if term := o.Supervise(ctx, live, fastPolicy(SupervisePolicy{
		Idle: config.Idle{Timeout: time.Minute, Action: config.IdleDestroy},
	})); term != nil {
		t.Fatalf("destroyed a rig mid-generation: %s", term.Summary)
	}
}

// FR-SUP-05 and §12.5: the ceiling must count storage, which is why the cost
// comes from the journal rather than from GPU time alone.
func TestBudgetCeilingDestroys(t *testing.T) {
	o, live := liveRig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	term := o.Supervise(ctx, live, fastPolicy(SupervisePolicy{
		Budget: config.Budget{MaxUSD: 0.0000001, Action: config.IdleDestroy},
	}))
	if term == nil {
		t.Fatal("a breached ceiling did not end the rig")
	}
	if term.Code != core.ReasonBudgetCeiling {
		t.Errorf("code = %s, want budget-ceiling", term.Code)
	}
	for _, k := range []string{"ceiling_usd", "total_usd", "storage_usd"} {
		if term.Evidence[k] == "" {
			t.Errorf("evidence missing %q", k)
		}
	}
}

// An operator interrupt is not a reason to destroy anything.
func TestInterruptDoesNotTerminate(t *testing.T) {
	o, live := liveRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if term := o.Supervise(ctx, live, fastPolicy(SupervisePolicy{
		Idle: config.Idle{Timeout: time.Nanosecond, Action: config.IdleDestroy},
	})); term != nil {
		t.Fatalf("an interrupt destroyed the rig: %s", term.Summary)
	}
}

// No policy configured means nothing is enforced — the supervisor must not
// invent a deadline the operator did not ask for.
func TestNoPolicyNeverTerminates(t *testing.T) {
	o, live := liveRig(t)
	live.proxy.Activity.MarkOperator(time.Now().Add(-24 * time.Hour))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if term := o.Supervise(ctx, live, fastPolicy(SupervisePolicy{})); term != nil {
		t.Fatalf("terminated with no policy set: %s", term.Summary)
	}
}
