// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"strings"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/state"
)

// The guard that keeps a dead process from turning into two invoices. An
// operator whose daemon died reaching for `up` again is the likeliest way to
// end up paying for two rigs, and the second one is invisible until it is
// billed.
func TestUpRefusesWhileARigIsStillBilling(t *testing.T) {
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	rig := &core.Rig{ID: newTestID(t), Offer: core.Offer{PriceHr: 0.44}}
	if err := st.Transition(rig, core.StateReady, "test"); err != nil {
		t.Fatal(err)
	}

	err = refuseIfAlreadyBilling(st)
	if err == nil {
		t.Fatal("a second rig would have been rented alongside a billing one")
	}
	// The message has to carry the way out, or it is an obstacle rather than
	// a safeguard.
	for _, want := range []string{rig.ID, "larri resume", "larri down", "0.440"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message should mention %q, got:\n%s", want, err)
		}
	}
}

// A destroyed rig is not a reason to refuse; the guard must not make the tool
// unusable after a normal teardown.
func TestUpProceedsWhenNothingIsBilling(t *testing.T) {
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	rig := &core.Rig{ID: newTestID(t)}
	if err := st.Transition(rig, core.StateDestroyed, "test"); err != nil {
		t.Fatal(err)
	}
	if err := refuseIfAlreadyBilling(st); err != nil {
		t.Errorf("refused with nothing billing: %v", err)
	}
}

func newTestID(t *testing.T) string {
	t.Helper()
	id, err := state.NewID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return id
}
