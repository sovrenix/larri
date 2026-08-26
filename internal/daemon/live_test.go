// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"strings"
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
	ssh := modelFailure{"Tesla V100", "daemon.waitForSSH"}
	o := &Orchestrator{failedModels: map[modelFailure]int{ssh: 1}}
	if got := o.retiredModels(); len(got) != 0 {
		t.Errorf("one failure is a bad box, not a pattern: got %v", got)
	}
	// Two failures of *different* kinds are two ordinary bad rentals, not
	// evidence about the model. A live run retired 151 RTX 3090 offers on
	// exactly this: one hardware-check failure and one unreachable host.
	o.failedModels[modelFailure{"Tesla V100", "daemon.verifyPlacedHardware"}] = 1
	if got := o.retiredModels(); len(got) != 0 {
		t.Errorf("unrelated failures must not retire a model: got %v", got)
	}
	o.failedModels[ssh] = modelStrikes
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

// nvidia-smi prints one line per GPU. Reading only the first compares a single
// card against a requirement the whole box is meant to satisfy — which
// rejected exactly the multi-GPU hosts that are the only affordable way to
// hold a 27B model. A live run paid to boot and pull the image on a 3x3090 box
// and then failed it for "24.0 GB but the plan needs 64.0 GB", and did the
// same on an 8x3060.
func TestGPUMemoryIsSummedAcrossCards(t *testing.T) {
	cases := []struct {
		name      string
		out       string
		wantMB    int
		wantCount int
	}{
		{"3x3090", "24576\n24576\n24576\n", 73728, 3},
		{"8x3060", strings.Repeat("12288\n", 8), 98304, 8},
		{"single card", "24576\n", 24576, 1},
		{"trailing blanks tolerated", "24576\n24576\n\n", 49152, 2},
		{"nothing reported", "\n\n", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mb, n := sumGPUMemoryMB(tc.out)
			if mb != tc.wantMB || n != tc.wantCount {
				t.Errorf("sumGPUMemoryMB = (%d MB, %d gpus), want (%d, %d)",
					mb, n, tc.wantMB, tc.wantCount)
			}
		})
	}
}
