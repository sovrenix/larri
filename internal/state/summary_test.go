// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package state

import (
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
)

func summaryRig() *core.Rig {
	return &core.Rig{
		ID: "r1", State: core.StateReady, LocalPort: 8000,
		Offer: core.Offer{Provider: "runpod", GPUModel: "A100 SXM", GPUCount: 2,
			VRAMPerGPUGB: 80, PriceHr: 2.78},
		Instance: &core.Instance{Provider: "runpod", InstanceID: "pod1", PriceHr: 3.18},
		Model:    core.ModelSpec{Ref: "org/m", Quantization: "Q4_K_M", ServedName: "m"},
		Runtime:  core.RuntimeLlamaCpp,
	}
}

// The provider's own rate wins once it has one, and the quote is kept beside
// it. A live RunPod pod quoted at $2.78/hr from the catalogue billed $3.18/hr;
// a summary carrying only one of the two hides that from every surface.
func TestSummaryShowsWhatTheProviderBillsAndWhatWasQuoted(t *testing.T) {
	s := Summarise(summaryRig(), nil, time.Now())
	if s.PriceHr != 3.18 || s.QuotedHr != 2.78 {
		t.Errorf("price %v quoted %v, want 3.18 and 2.78", s.PriceHr, s.QuotedHr)
	}
	if !s.PriceDiffers() {
		t.Error("a 40-cent gap was not reported as a difference")
	}
	if s.Hardware != "2× A100 SXM 160GB" {
		t.Errorf("hardware %q; the card count is part of what was rented", s.Hardware)
	}
	if s.Instance != "pod1" || s.Provider != "runpod" {
		t.Errorf("instance %q provider %q", s.Instance, s.Provider)
	}
}

// A rig with no instance still has a provider and hardware — the offer it was
// going to rent. Dropping them, as the MCP tool did, left an agent looking at a
// rig it could not place.
func TestSummaryWithoutAnInstanceKeepsTheOffer(t *testing.T) {
	r := summaryRig()
	r.Instance, r.State = nil, core.StateFailed
	s := Summarise(r, nil, time.Now())
	if s.Provider != "runpod" || s.Hardware == "" {
		t.Errorf("provider %q hardware %q for a rig with no instance", s.Provider, s.Hardware)
	}
	if s.Instance != "" {
		t.Errorf("instance %q invented for a rig with none recorded", s.Instance)
	}
	if s.PriceHr != 2.78 || s.PriceDiffers() {
		t.Errorf("price %v; with nothing billed, the quote is the rate", s.PriceHr)
	}
}

// The endpoint is shown only while the rig can answer on it. A destroyed
// rig's port belongs to whatever binds it next.
func TestSummaryEndpointOnlyWhileServing(t *testing.T) {
	r := summaryRig()
	if s := Summarise(r, nil, time.Now()); s.Endpoint != "http://127.0.0.1:8000/v1" {
		t.Errorf("endpoint %q for a READY rig", s.Endpoint)
	}
	r.State = core.StateDestroyed
	if s := Summarise(r, nil, time.Now()); s.Endpoint != "" {
		t.Errorf("endpoint %q for a destroyed rig", s.Endpoint)
	}
}
