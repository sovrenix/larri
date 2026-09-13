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

// The suggestions had no target to measure against, so the first alternative
// counted as fitting whatever its size and the loop stopped there. A live run
// answered a 121.7 GB shortfall with "try --quantization q8_0 (~212.6 GB)" —
// ninety gigabytes larger than the problem it was offered as the fix for.
func TestSuggestionsAreMeasuredAgainstTheMarket(t *testing.T) {
	f := Facts{Params: 70, Layers: 80, KVHeads: 8, HeadDim: 128,
		HiddenSize: 8192, MaxContextLen: 131072}
	offers := []core.Offer{
		{Provider: "p", OfferID: "a", GPUModel: "RTX 4090", GPUCount: 1,
			VRAMPerGPUGB: 24, PriceHr: 0.40},
	}
	s := Analyse(Request{Spec: spec("fp16", 8192), Facts: f}, offers)
	for _, sg := range s.Suggestions {
		if sg.Fits && sg.RequiredB > s.BestVRAMB {
			t.Errorf("%s (~%s) offered as a fix for a market with %s usable",
				sg.Flag, HumanBytes(sg.RequiredB), HumanBytes(s.BestVRAMB))
		}
	}
	if s.RequiredB == 0 {
		t.Fatal("the requirement was not computed")
	}
}

// The count is part of an offer's identity once multi-GPU hosts are ranked.
// "RTX PRO 6000 96GB" is one card or four depending on the listing, and a
// report that omits which cannot be read.
func TestShortfallNamesTheCardCount(t *testing.T) {
	f := Facts{Params: 235, Layers: 94, KVHeads: 4, HeadDim: 128,
		HiddenSize: 4096, MaxContextLen: 131072}
	offers := []core.Offer{
		{Provider: "p", OfferID: "quad", GPUModel: "RTX 4090", GPUCount: 4,
			VRAMPerGPUGB: 24, PriceHr: 1.60},
	}
	s := Analyse(Request{Spec: spec("q4_K_M", 8192), Facts: f}, offers)
	out := s.String()
	if !strings.Contains(out, "4× RTX 4090 96GB") {
		t.Errorf("the report does not say how many cards it weighed:\n%s", out)
	}
	if !strings.Contains(out, "single or multi-GPU") {
		t.Errorf("a multi-GPU market reported as though only single cards were looked at:\n%s", out)
	}
}

// A host whose cards the engine cannot all reach is measured on the ones it
// can. Sizing against the advertised total is how a six-card box gets rented
// for a model only all six could have held.
func TestShortfallMeasuresOnlyTheCardsTheEngineReaches(t *testing.T) {
	f := Facts{Params: 70, Layers: 80, AttentionHeads: 40, KVHeads: 8, HeadDim: 128,
		HiddenSize: 8192, MaxContextLen: 131072}
	offers := []core.Offer{
		{Provider: "p", OfferID: "six", GPUModel: "RTX 3090", GPUCount: 6,
			VRAMPerGPUGB: 24, PriceHr: 1.20},
	}
	req := Request{Spec: spec("fp16", 8192), Facts: f,
		Shards: func(gpus int) int { return Shards(f, gpus, true) }}
	s := Analyse(req, offers)
	// 40 heads shard across four cards, not six.
	if want := ShardVRAM(24, 4); s.BestVRAMB != want {
		t.Errorf("weighed %s of a six-card host; the engine reaches four, which is %s",
			HumanBytes(s.BestVRAMB), HumanBytes(want))
	}
}

// The measurement belongs to the file the operator asked for. Carrying it into
// a different quantisation would price every alternative as the one that
// already did not fit, so the report would say nothing helps whatever the
// repository actually offers.
func TestSuggestedQuantisationsDoNotInheritTheMeasurement(t *testing.T) {
	f := Facts{Params: 70, Layers: 80, KVHeads: 8, HeadDim: 128,
		HiddenSize: 8192, MaxContextLen: 131072}
	offers := []core.Offer{
		{Provider: "p", OfferID: "a", GPUModel: "A100", GPUCount: 1,
			VRAMPerGPUGB: 80, PriceHr: 1.29},
	}
	// fp16 measured at 140 GB: too large for an 80 GB card.
	s := Analyse(Request{Spec: spec("fp16", 8192), Facts: f,
		WeightBytes: 140 << 30}, offers)
	if s.RequiredB <= 140<<30 {
		t.Fatalf("the measurement did not reach the requirement: %s",
			HumanBytes(s.RequiredB))
	}
	var sized int
	for _, sg := range s.Suggestions {
		if strings.HasPrefix(sg.Flag, "--quantization") {
			sized++
			if sg.RequiredB >= s.RequiredB {
				t.Errorf("%s costs %s, no less than the %s that did not fit — "+
					"the fp16 measurement was carried across",
					sg.Flag, HumanBytes(sg.RequiredB), HumanBytes(s.RequiredB))
			}
		}
	}
	if sized == 0 {
		t.Error("no quantisation was suggested at all")
	}
}

// "single or multi-GPU" is a claim about what was weighed. Under a one-card
// ceiling no multi-GPU host was, and the report must not say otherwise.
func TestShortfallClaimsMultiGPUOnlyWhenItWeighedSome(t *testing.T) {
	f := Facts{Params: 235, Layers: 94, KVHeads: 4, HeadDim: 128,
		HiddenSize: 4096, MaxContextLen: 131072}
	single := []core.Offer{{Provider: "p", OfferID: "a", GPUModel: "H100", GPUCount: 1,
		VRAMPerGPUGB: 80, PriceHr: 2.69}}
	out := Analyse(Request{Spec: spec("q4_K_M", 8192), Facts: f}, single).String()
	if strings.Contains(out, "multi-GPU") {
		t.Errorf("claimed multi-GPU hosts were weighed when none were:\n%s", out)
	}
	if strings.Contains(out, ", (") {
		t.Errorf("dropping the phrase left its comma behind:\n%s", out)
	}
}
