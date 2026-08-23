// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package tui

import (
	"strings"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/term"
)

func send(m Model, msgs ...term.Msg) Model {
	for _, msg := range msgs {
		next, _ := m.Update(msg)
		m = next.(Model)
	}
	return m
}

func servingModel() Model {
	m := New()
	return send(m, ReadyMsg{
		Rig: &core.Rig{
			ID: "01TEST", Model: core.ModelSpec{Ref: "org/model", ServedName: "m"},
			Offer:   core.Offer{GPUModel: "RTX 4090", VRAMPerGPUGB: 24, GPUCount: 1, PriceHr: 0.44},
			Runtime: core.RuntimeVLLM, State: core.StateReady,
		},
		Endpoint: "http://127.0.0.1:8000/v1", Token: "tok",
	})
}

// Quitting must tear down. A dashboard that exited while the rig kept billing
// would make leaving look free, which is the failure this whole product is
// about.
func TestQuittingTearsDown(t *testing.T) {
	var quit bool
	m := servingModel()
	m.Quit = func() { quit = true }

	m = send(m, term.KeyMsg{Type: term.KeyRunes, Runes: []rune{'q'}})
	if !quit {
		t.Fatal("quitting left the rig billing")
	}
	if m.Phase != PhaseTearingDown {
		t.Errorf("phase = %v, want tearing down", m.Phase)
	}
}

// Destroying rented hardware takes a confirmation, and a stray keypress must
// not be one.
func TestDestroyNeedsConfirmation(t *testing.T) {
	var destroyed bool
	m := servingModel()
	m.Destroy = func() { destroyed = true }

	m = send(m, term.KeyMsg{Type: term.KeyRunes, Runes: []rune{'d'}})
	if destroyed {
		t.Fatal("one keypress destroyed a rig")
	}
	if !strings.Contains(m.View(), "destroy this rig?") {
		t.Error("no confirmation was shown")
	}
	m = send(m, term.KeyMsg{Type: term.KeyRunes, Runes: []rune{'n'}})
	if destroyed {
		t.Fatal("declining still destroyed the rig")
	}
	m = send(m, term.KeyMsg{Type: term.KeyRunes, Runes: []rune{'d'}},
		term.KeyMsg{Type: term.KeyRunes, Runes: []rune{'y'}})
	if !destroyed {
		t.Error("confirming did not destroy")
	}
}

// The numbers that matter must be on screen: what it costs now, and the
// storage half that keeps billing after the GPU is released.
func TestServingViewShowsCostAndActivity(t *testing.T) {
	m := send(servingModel(), StatsMsg{
		Accrued:  core.CostSummary{TotalUSD: 0.1234, ComputeUSD: 0.1000, StorageUSD: 0.0234},
		Idle:     90 * time.Second,
		Requests: 7, Probes: 42,
		Healthy: true,
	})
	v := m.View()
	for _, want := range []string{"$0.1234", "compute $0.1000", "storage $0.0234",
		"1m30s", "RTX 4090", "$0.440/hr", "http://127.0.0.1:8000/v1"} {
		if !strings.Contains(v, want) {
			t.Errorf("view is missing %q:\n%s", want, v)
		}
	}
	// FR-SUP-08 made visible: the operator can see that health probes are
	// counted separately and do not hold the rig alive.
	if !strings.Contains(v, "health probes, excluded from idle") {
		t.Error("the probe exclusion is not shown")
	}
}

// A long bring-up must look like progress, not like a hang (FR-RT-06).
func TestProvisioningShowsTheEventTail(t *testing.T) {
	m := New()
	for i := 0; i < 300; i++ {
		m = send(m, EventMsg(daemon.Event{Phase: "boot", Message: "pulling image"}))
	}
	m = send(m, EventMsg(daemon.Event{Phase: "boot", Message: "weights 61%"}))
	v := m.View()
	if !strings.Contains(v, "weights 61%") {
		t.Error("the newest event is not shown")
	}
	// Bounded, or a 15 GB pull would grow the buffer without limit.
	if len(m.events) > 200 {
		t.Errorf("event buffer grew to %d", len(m.events))
	}
}

// The cost has to survive the dashboard closing.
func TestDoneSummaryCarriesTheTermination(t *testing.T) {
	m := send(servingModel(), DoneMsg{
		Term: &core.Termination{
			Code: core.ReasonIdleTimeout, Summary: "no operator inference for 31m",
			Evidence: map[string]string{"window": "30m"},
		},
		Cost: core.CostSummary{TotalUSD: 2.87, Ran: 2 * time.Hour},
	})
	v := m.Done()
	for _, want := range []string{"idle-timeout", "no operator inference for 31m", "$2.8700", "window"} {
		if !strings.Contains(v, want) {
			t.Errorf("summary missing %q:\n%s", want, v)
		}
	}
}
