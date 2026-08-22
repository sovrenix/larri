// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package fake

import (
	"context"
	"testing"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
)

func spec() core.ModelSpec {
	return core.ModelSpec{Ref: "Qwen/Qwen3-Coder-30B", ServedName: "qwen3-coder"}
}

// FR-SEC-08: not configurable means rejected, not warned about.
func TestNonLoopbackBindIsRejectedAtLaunch(t *testing.T) {
	r := New(Behaviour{BindHost: "0.0.0.0"})
	_, err := r.Launch(context.Background(), &Session{}, spec(), core.SizingPlan{})
	if err == nil {
		t.Fatal("binding a routable interface must fail, not warn")
	}
	ep := runtime.Endpoint{Host: "0.0.0.0"}
	if ep.Valid() {
		t.Fatal("Endpoint.Valid must reject a routable bind address")
	}
	if !(runtime.Endpoint{Host: runtime.Loopback}).Valid() {
		t.Fatal("loopback must be accepted")
	}
}

// NFR-05: READY means a completion round-tripped. A server that accepts
// connections but never completes is the case that makes a TCP check useless.
func TestOpenPortIsNotReadiness(t *testing.T) {
	r := New(Behaviour{NeverReady: true})
	ep, err := r.Launch(context.Background(), &Session{}, spec(), core.SizingPlan{})
	if err != nil {
		t.Fatalf("launch should succeed — the port opens: %v", err)
	}
	if err := r.Ready(context.Background(), ep, spec()); err == nil {
		t.Fatal("readiness must require a completion, not an open port")
	}
}

// FR-PROV-05: host failures fall back to the next offer; model failures must
// not, because the next host fails identically. The distinction is carried by
// the error class, so that is what is asserted.
func TestFailureClassDecidesWhetherFallbackIsSane(t *testing.T) {
	hostFail := New(Behaviour{BootstrapFails: true})
	err := hostFail.Bootstrap(context.Background(), &Session{}, spec(), core.SizingPlan{}, nil)
	if !errs.Is(err, errs.ClassHostFailure) {
		t.Fatalf("bootstrap failure class = %s, want host-failure (retry elsewhere)", errs.ClassOf(err))
	}

	modelFail := New(Behaviour{OOMAtLoad: true})
	_, err = modelFail.Launch(context.Background(), &Session{}, spec(), core.SizingPlan{})
	if !errs.Is(err, errs.ClassModelFailure) {
		t.Fatalf("OOM class = %s, want model-failure (do not retry elsewhere)", errs.ClassOf(err))
	}
}

// FR-RT-06: a multi-GB download must not look like a hang.
func TestBootstrapReportsByteProgress(t *testing.T) {
	r := New(Behaviour{WeightBytes: 19_100_000_000})
	ch := make(chan runtime.Progress, 16)
	done := make(chan error, 1)
	go func() {
		done <- r.Bootstrap(context.Background(), &Session{}, spec(), core.SizingPlan{}, ch)
		close(ch)
	}()
	var last runtime.Progress
	sawBytes := false
	for p := range ch {
		if p.Phase == runtime.PhaseWeightsDownload && p.BytesTotal > 0 {
			sawBytes = true
			last = p
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !sawBytes {
		t.Fatal("weight download must report bytes, not just a phase")
	}
	if last.Percent != 100 || last.BytesDone != last.BytesTotal {
		t.Errorf("final progress = %.0f%% %d/%d, want complete",
			last.Percent, last.BytesDone, last.BytesTotal)
	}
}

func TestEndpointKeyIsRedacted(t *testing.T) {
	r := New(Behaviour{})
	ep, err := r.Launch(context.Background(), &Session{}, spec(), core.SizingPlan{})
	if err != nil {
		t.Fatal(err)
	}
	if ep.Key.Empty() {
		t.Fatal("the runtime must be launched with a rig token (FR-SEC-08)")
	}
	if got := ep.Key.String(); got != "***" {
		t.Errorf("endpoint key must redact when formatted, got %q", got)
	}
}
