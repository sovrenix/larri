// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"testing"

	"go.sovrenix.com/larri/internal/core"
)

// A live run rented three V100 boxes in a row and abandoned each one while it
// was still pulling the image. Vast publishes the SSH endpoint when the
// contract starts — before the container exists — so the reachability window
// opened at a moment when sshd could not possibly answer, and closed three
// minutes later on hosts that were working the whole time.
func TestBootPendingSeparatesPullingFromDead(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"loading", true}, // vast: the image is still arriving
		{"created", true},
		// Billing has started and the container has not been reported at
		// all. Ambiguous — and a live run watched one sit that way for ten
		// minutes against an address nothing was listening on, so the
		// window runs.
		{"contract running", false},
		{"", false},
		{"running", false}, // the case the window exists to catch
		{"exited", false},
		{"offline", false},
	}
	for _, tc := range cases {
		if got := bootPending(&core.Instance{Status: tc.status}); got != tc.want {
			t.Errorf("bootPending(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// Machine-level exclusion is not enough against a deep pool of identical
// boxes: price-dominated ranking walks back into it every time, because a
// fresh cheap box always outranks a dearer working one.
func TestRepeatedModelFailuresRetireTheModel(t *testing.T) {
	o := &Orchestrator{failedModels: map[string]int{"Tesla V100": 1}}
	if got := o.retiredModels(); len(got) != 0 {
		t.Errorf("one failure is a bad box, not a pattern: got %v", got)
	}
	o.failedModels["Tesla V100"] = modelStrikes
	if got := o.retiredModels(); len(got) != 1 || got[0] != "Tesla V100" {
		t.Errorf("retiredModels() = %v, want [Tesla V100]", got)
	}

	offers := []core.Offer{
		{GPUModel: "Tesla V100", PriceHr: 0.109},
		{GPUModel: "A100", PriceHr: 0.90},
		{GPUModel: "Tesla V100", PriceHr: 0.110},
	}
	kept := withoutModels(offers, []string{"Tesla V100"})
	if len(kept) != 1 || kept[0].GPUModel != "A100" {
		t.Fatalf("withoutModels kept %v", kept)
	}
}
