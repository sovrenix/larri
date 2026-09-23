// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"testing"
	"time"

	pfake "go.sovrenix.com/larri/internal/provider/fake"
	"go.sovrenix.com/larri/internal/runtime"
	rfake "go.sovrenix.com/larri/internal/runtime/fake"
)

// browserWorkload is an engine that serves something other than /v1.
//
// Declared here rather than borrowed from a real adapter so this file stays
// free of any particular workload: what is under test is the wiring layer's
// reaction to a protocol, not any one implementation of one.
type browserWorkload struct{ runtime.Runtime }

func (browserWorkload) Protocol() runtime.Protocol { return runtime.ProtocolComfyUI }

// A live run set --budget 0.50 on a $0.34/hr host, hung during launch, and was
// on course for $0.51 with nothing to show — because budget enforcement lived
// only in Supervise, which starts once a rig is READY. A ceiling that does not
// apply while spending is not a ceiling.
func TestTheBudgetBoundsBringUp(t *testing.T) {
	o, _, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	o.BudgetUSD = 0.50

	// $0.10 already spent on a failed attempt, $0.34/hr, nothing elapsed:
	// $0.40 remains, which buys about seventy minutes.
	ctx, cancel := o.budgetBounded(context.Background(), 0.34, 0.10, time.Now())
	defer cancel()

	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("the budget set no bound at all")
	}
	remaining, price := 0.40, 0.34
	want := time.Duration(remaining / price * float64(time.Hour))
	if got := time.Until(dl); got < want-time.Minute || got > want+time.Minute {
		t.Errorf("bound = %s, want about %s", got.Round(time.Second), want.Round(time.Second))
	}
}

// Past the ceiling already, the attempt must end rather than run on to the
// provisioning deadline.
func TestAnExhaustedBudgetEndsTheAttemptAtOnce(t *testing.T) {
	o, _, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	o.BudgetUSD = 0.10

	ctx, cancel := o.budgetBounded(context.Background(), 0.34, 0.50, time.Now())
	defer cancel()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a bring-up past its budget kept running")
	}
}

// It only ever tightens. A generous budget leaves the provisioning deadline in
// charge, which is what every rig did before this existed.
func TestAGenerousBudgetDoesNotLoosenTheDeadline(t *testing.T) {
	o, _, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	o.BudgetUSD = 1000

	parent, pcancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer pcancel()
	ctx, cancel := o.budgetBounded(parent, 0.34, 0, time.Now())
	defer cancel()

	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("the parent deadline was lost")
	}
	if time.Until(dl) > 31*time.Minute {
		t.Errorf("the budget extended the deadline to %s", time.Until(dl))
	}
}

// With no budget set, nothing changes.
func TestNoBudgetLeavesTheContextAlone(t *testing.T) {
	o, _, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	ctx, cancel := o.budgetBounded(context.Background(), 0.34, 0, time.Now())
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Error("a bound appeared without a budget to justify it")
	}
}

// "/v1" is the OpenAI base path an inference client is configured with. A
// browser surface has no such base, so a live run printed
// "tunnel http://127.0.0.1:8188/v1" naming a path that 404s — which is the
// first thing an operator would have pasted into a browser.
func TestTheEndpointURLFollowsTheProtocol(t *testing.T) {
	o, _, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})

	// An engine keeps /v1, because every wired client depends on that string.
	if got := o.endpointURL(8000); got != "http://127.0.0.1:8000/v1" {
		t.Errorf("openai endpoint = %q, want the /v1 base", got)
	}

	o.Runtime = browserWorkload{rfake.New(rfake.Behaviour{})}
	if got := o.endpointURL(8188); got != "http://127.0.0.1:8188/" {
		t.Errorf("browser endpoint = %q, want the root", got)
	}

	// And a rig with no workload yet still names something usable.
	o.Runtime = nil
	if got := o.endpointURL(8000); got != "http://127.0.0.1:8000/v1" {
		t.Errorf("default endpoint = %q", got)
	}
}

// The ceiling must not be the one exit that leaves a rig billing.
//
// The budget context tightens Serve, so exhausting the ceiling is a likely way
// for an attempt to fail — and attempt returns the rig with its instance still
// alive, because Live.Close releases the tunnel and never the machine. While
// the budget stop sat above the common teardown it skipped it entirely, so the
// control whose whole purpose is to stop the spending was the single path that
// left a GPU running until somebody noticed.
func TestStoppingOnTheBudgetStillDestroysTheRig(t *testing.T) {
	o, p, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	// Small enough that any elapsed second exhausts it, so Serve is cancelled
	// by the budget rather than by the provisioning deadline.
	o.BudgetUSD = 1e-9

	live, err := o.UpAndServe(context.Background(), upReq())
	if err == nil {
		t.Fatal("a bring-up past its ceiling reported success")
	}
	if live != nil {
		t.Error("a failed bring-up returned a live session")
	}

	inst, lerr := p.List(context.Background())
	if lerr != nil {
		t.Fatal(lerr)
	}
	for _, i := range inst {
		t.Errorf("instance %s is still alive after the budget stopped the run: "+
			"the ceiling left a rig billing", i.InstanceID)
	}
}
