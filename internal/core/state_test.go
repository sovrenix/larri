// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package core

import "testing"

// The single most expensive mistake this package can make is treating a state
// that costs money as free. R-13: a stopped instance still exists, so storage
// still bills, and only absence from the provider's inventory ends it.
func TestBillability(t *testing.T) {
	billable := []LifecycleState{
		StateCreating, // assume yes: the call may have landed
		StateProvisioned,
		StateBootstrapping,
		StateReady,
		StateDegraded,
		StateStopped, // <- the trap: not running, still charging storage
		StateDraining,
		StateFailed, // assume yes: may still have an instance behind it
		StateOrphaned,
	}
	free := []LifecycleState{StateIdle, StateSearching, StateSelected, StateDestroyed}

	for _, s := range billable {
		if !s.Billable() {
			t.Errorf("%s must be treated as billable", s)
		}
	}
	for _, s := range free {
		if s.Billable() {
			t.Errorf("%s must not be treated as billable", s)
		}
	}
}

func TestOnlyDestroyedIsTerminal(t *testing.T) {
	if !StateDestroyed.Terminal() {
		t.Error("DESTROYED must be terminal")
	}
	// STOPPED sounds terminal and is not; it needs a destroy decision.
	if StateStopped.Terminal() {
		t.Error("STOPPED must not be terminal: it still bills and still needs a decision")
	}
	if StateFailed.Terminal() {
		t.Error("FAILED must not be terminal: teardown may still have an instance to reach")
	}
}

// Q-04: interruptible offers are opt-in, so the zero value must forbid them.
func TestTristateZeroValueForbids(t *testing.T) {
	var unset Tristate
	if unset != Forbid {
		t.Fatalf("zero Tristate = %v, want forbid — an unset criterion must be conservative", unset)
	}
	if unset.Permits(true) {
		t.Error("an unset Interruptible must reject an interruptible offer")
	}
	if !unset.Permits(false) {
		t.Error("an unset Interruptible must accept a non-interruptible offer")
	}
}

func TestTristatePermits(t *testing.T) {
	cases := []struct {
		t    Tristate
		has  bool
		want bool
	}{
		{Require, true, true}, {Require, false, false},
		{Allow, true, true}, {Allow, false, true},
		{Forbid, true, false}, {Forbid, false, true},
	}
	for _, c := range cases {
		if got := c.t.Permits(c.has); got != c.want {
			t.Errorf("%s.Permits(%v) = %v, want %v", c.t, c.has, got, c.want)
		}
	}
}

func TestRigIDFromLabel(t *testing.T) {
	ours := Instance{Labels: map[string]string{LabelKey: "01J9Z"}}
	if id, ok := ours.RigID(); !ok || id != "01J9Z" {
		t.Errorf("RigID() = %q,%v; want 01J9Z,true", id, ok)
	}
	// An operator's own instance carries no marker and must not be claimed.
	theirs := Instance{Labels: map[string]string{"owner": "someone-else"}}
	if _, ok := theirs.RigID(); ok {
		t.Error("an unlabelled instance must not be attributed to LARRI")
	}
	if _, ok := (Instance{}).RigID(); ok {
		t.Error("an instance with no labels must not be attributed to LARRI")
	}
}
