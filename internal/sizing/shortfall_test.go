// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sizing

import (
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/core"
)

// NFR-11 names three things the most common failure message must contain: the
// VRAM required, the VRAM found, and the cheapest offer that would fit.
func TestShortfallStatesRequiredFoundAndCheapestFit(t *testing.T) {
	req := Request{
		Spec:               spec("fp16", 32768),
		Facts:              llama70B,
		AvailableVRAMBytes: 24 * GiB,
	}
	offers := []core.Offer{
		{GPUModel: "RTX 4090", GPUCount: 1, VRAMPerGPUGB: 24, PriceHr: 0.34},
		{GPUModel: "A100", GPUCount: 1, VRAMPerGPUGB: 80, PriceHr: 1.29},
		{GPUModel: "H100", GPUCount: 2, VRAMPerGPUGB: 80, PriceHr: 4.10},
	}
	s := Analyse(req, offers)
	out := s.String()
	t.Logf("\n%s", out)

	if s.RequiredB == 0 {
		t.Fatal("the requirement must be computed")
	}
	if !strings.Contains(out, "needs ~") {
		t.Error("message must state the VRAM required")
	}
	if s.Best == nil || !strings.Contains(out, "Best matching offer") {
		t.Error("message must state what was found")
	}
	if !strings.Contains(out, "short") {
		t.Error("message must quantify the shortfall, not just report failure")
	}
	// 70B at fp16 and 32k context needs ~169 GB — more than 2x80 GB. Nothing
	// on this table fits, and the message must say so rather than fall silent,
	// which would read as "we did not look".
	if s.CheapestFit != nil {
		t.Errorf("nothing here fits; CheapestFit = %s", s.CheapestFit.GPUModel)
	}
	if !strings.Contains(out, "No offer on the table") {
		t.Errorf("an unfittable request must say so explicitly, got:\n%s", out)
	}
}

// The same request against hardware that can hold it must name the cheapest
// one, which is the half of NFR-11 the case above cannot exercise.
func TestShortfallNamesTheCheapestOfferThatFits(t *testing.T) {
	req := Request{Spec: spec("q4_K_M", 8192), Facts: llama70B, AvailableVRAMBytes: 24 * GiB}
	offers := []core.Offer{
		{GPUModel: "RTX 4090", GPUCount: 1, VRAMPerGPUGB: 24, PriceHr: 0.34},
		{GPUModel: "A100", GPUCount: 1, VRAMPerGPUGB: 80, PriceHr: 1.29},
		{GPUModel: "H100", GPUCount: 1, VRAMPerGPUGB: 80, PriceHr: 2.90},
	}
	s := Analyse(req, offers)
	out := s.String()
	t.Logf("\n%s", out)

	if s.CheapestFit == nil {
		t.Fatal("70B q4 needs ~50 GB; the 80 GB cards fit and one must be named")
	}
	if s.CheapestFit.GPUModel != "A100" {
		t.Errorf("cheapest fit = %s @ $%.2f, want the A100 at $1.29 — cheapest, not first",
			s.CheapestFit.GPUModel, s.CheapestFit.PriceHr)
	}
}

// The operator's next action is almost always to quantise or shorten context,
// so the message tells them which one works rather than leaving them to guess.
func TestShortfallSuggestsAWorkingAlternative(t *testing.T) {
	req := Request{
		Spec:               spec("fp16", 32768),
		Facts:              llama70B,
		AvailableVRAMBytes: 80 * GiB,
	}
	s := Analyse(req, []core.Offer{{GPUModel: "A100", GPUCount: 1, VRAMPerGPUGB: 80, PriceHr: 1.29}})
	out := s.String()
	t.Logf("\n%s", out)

	if !strings.Contains(out, "Try:") {
		t.Fatalf("a fixable shortfall must suggest a fix, got:\n%s", out)
	}
	var workable int
	for _, sg := range s.Suggestions {
		if sg.Fits {
			workable++
			if sg.RequiredB > req.AvailableVRAMBytes {
				t.Errorf("suggestion %q claims to fit but needs %s of %s",
					sg.Flag, HumanBytes(sg.RequiredB), HumanBytes(req.AvailableVRAMBytes))
			}
		}
	}
	if workable == 0 {
		t.Error("70B on an 80 GB card is fixable by quantisation; a suggestion should say so")
	}
}

func TestShortfallWithNoCandidatesSaysSo(t *testing.T) {
	s := Analyse(Request{Spec: spec("fp16", 8192), Facts: llama70B}, nil)
	out := s.String()
	if !strings.Contains(out, "No offer satisfied") {
		t.Errorf("an empty candidate set must be explained, got:\n%s", out)
	}
}
