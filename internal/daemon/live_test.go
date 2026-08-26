// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"go.sovrenix.com/larri/internal/errs"
	"net"
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

// The failure every timeout in waitForSSH was blind to. A live run watched an
// instance report actual_status "created" — indistinguishable from a boot in
// progress — while intended_status had already flipped to "stopped" because
// the GPUs could not be attached to the container:
//
//	OCI runtime create failed: ... unresolvable CDI devices
//
// LARRI printed "image still arriving" at it and waited, while the provider's
// own console showed Inactive and the error.
func TestProviderGivingUpIsRecognisedImmediately(t *testing.T) {
	cases := []struct {
		name string
		inst core.Instance
		want bool
	}{
		{"oci create failure", core.Instance{Status: "created", Intent: "stopped"}, true},
		{"exited", core.Instance{Status: "loading", Intent: "exited"}, true},
		{"still trying", core.Instance{Status: "loading", Intent: "running"}, false},
		{"intent unreported", core.Instance{Status: "loading"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bootAbandoned(&tc.inst); got != tc.want {
				t.Errorf("bootAbandoned = %v, want %v", got, tc.want)
			}
		})
	}

	// "created" is a pending status, so without the intent check the wait
	// would keep going. The two signals must be read together.
	stuck := &core.Instance{Status: "created", Intent: "stopped"}
	if !bootPending(stuck) {
		t.Error("precondition: 'created' reads as pending, which is why intent is needed")
	}

	// The provider's own message is what names the cause, so it must survive
	// into the error an operator sees.
	msg := "Error response from daemon: OCI runtime create failed: unresolvable CDI devices"
	got := bootFailureReason(&core.Instance{Status: "created", Intent: "stopped", StatusMsg: msg})
	if !strings.Contains(got, "CDI devices") {
		t.Errorf("reason should carry the provider's message, got %q", got)
	}
	// And with no message, it still says something usable.
	if r := bootFailureReason(&core.Instance{Status: "created", Intent: "stopped"}); !strings.Contains(r, "created") {
		t.Errorf("reason without a message = %q", r)
	}
}

// A live run rented a GPU, booted it, launched vLLM and began pulling weights
// before discovering that something on the operator's own machine already held
// port 8000 — knowable before the first API call, and not fixable by any other
// host. Preconditions belong before the money.
func TestLocalPortIsCheckedBeforeSpending(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	taken := ln.Addr().(*net.TCPAddr).Port

	if err := checkLocalPort(taken); err == nil {
		t.Fatal("an occupied port must be refused")
	} else {
		if !errs.Is(err, errs.ClassWiring) {
			t.Errorf("class = %s, want wiring so it is not retried on another host", errs.ClassOf(err))
		}
		// The convention: append a remedy where one exists.
		if !strings.Contains(err.Error(), "--port") {
			t.Errorf("the error should name the way out: %v", err)
		}
	}

	// A free port passes, and the check must not leave the port held.
	ln.Close()
	if err := checkLocalPort(taken); err != nil {
		t.Errorf("a free port must pass: %v", err)
	}
	if err := checkLocalPort(taken); err != nil {
		t.Errorf("the check must release the port it tested: %v", err)
	}
	// Port 0 means "anything free", which cannot collide.
	if err := checkLocalPort(0); err != nil {
		t.Errorf("port 0 must pass: %v", err)
	}
}

// Three rentals died at the eight-minute stall timeout with their image pull
// most of the way done. Vast's status_msg is a rolling buffer of docker's
// output and new progress is appended to the END, but the stall comparison
// used describeBoot, which truncates to the first 120 characters for display.
// The prefix sat unchanged while gigabytes moved, and the timer read that as
// silence.
func TestStallDetectionReadsTheWholeMessageNotTheDisplayedPrefix(t *testing.T) {
	// A buffer whose first 120 characters are identical and whose tail is
	// where the pull is actually reported.
	head := strings.Repeat("526d5438c009: Download complete\n", 6) // >120 chars
	a := &core.Instance{Status: "loading", StatusMsg: head + "e2c6bed4fdc3: Downloading 2.1GB/12GB"}
	b := &core.Instance{Status: "loading", StatusMsg: head + "e2c6bed4fdc3: Downloading 7.8GB/12GB"}

	if describeBoot(a) == describeBoot(b) {
		// Not a failure in itself — it is the precondition that made the bug
		// invisible, and it is why the signature exists.
		t.Log("display is identical for both, as it was live")
	}
	if bootSignature(a) == bootSignature(b) {
		t.Fatal("a pull that moved 5.7GB must not read as no progress")
	}

	// Disk growth is progress even when the provider's text does not move.
	c := &core.Instance{Status: "loading", StatusMsg: "same", DiskUsedGB: 3.0}
	d := &core.Instance{Status: "loading", StatusMsg: "same", DiskUsedGB: 9.5}
	if bootSignature(c) == bootSignature(d) {
		t.Error("bytes landing on disk must count as progress")
	}
	// And a genuinely stalled host still reads as stalled.
	if bootSignature(c) != bootSignature(&core.Instance{Status: "loading", StatusMsg: "same", DiskUsedGB: 3.0}) {
		t.Error("an unchanged host must produce an unchanged signature")
	}
}

// The buffer accumulates, so its first line is the oldest news in it.
func TestBootDisplayShowsTheNewestLine(t *testing.T) {
	inst := &core.Instance{
		Status:    "loading",
		StatusMsg: "cf57d2112d89: Pulling fs layer\nc567a87f21d2: Pulling fs layer\ncd634bc724c4: Download complete\n",
	}
	got := describeBoot(inst)
	if !strings.Contains(got, "cd634bc724c4: Download complete") {
		t.Errorf("should report the newest line, got %q", got)
	}
	if strings.Contains(got, "cf57d2112d89") {
		t.Errorf("should not report minutes-old news as current: %q", got)
	}
}
