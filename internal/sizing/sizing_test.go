// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sizing

import (
	"math"
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/core"
)

// Real architectures, so the expected figures below are checkable against the
// published configs rather than against this implementation.
var (
	llama8B = Facts{
		Ref:    "meta-llama/Llama-3.1-8B-Instruct",
		Params: 8.03, Layers: 32, KVHeads: 8, HeadDim: 128,
		HiddenSize: 4096, MaxContextLen: 131072,
	}
	llama70B = Facts{
		Ref:    "meta-llama/Llama-3.1-70B-Instruct",
		Params: 70.6, Layers: 80, KVHeads: 8, HeadDim: 128,
		HiddenSize: 8192, MaxContextLen: 131072,
	}
)

func spec(quant string, ctx int) core.ModelSpec {
	return core.ModelSpec{Ref: "test", Quantization: quant, ContextLen: ctx}
}

func within(t *testing.T, label string, got uint64, wantGiB, tolerance float64) {
	t.Helper()
	gotGiB := float64(got) / GiB
	if math.Abs(gotGiB-wantGiB)/wantGiB > tolerance {
		t.Errorf("%s = %.2f GiB, want %.2f GiB (±%.0f%%)",
			label, gotGiB, wantGiB, tolerance*100)
	}
}

// The headline case, and the one the ranking function depends on being right:
// a 70B at fp16 does not fit an 80 GB card, and the same model at q4_K_M does.
// Getting this backwards either rejects a workable rig or rents one that OOMs.
func TestQuantizationDecidesWhether70BFitsAnA100(t *testing.T) {
	const a100 = 80 * GiB

	fp16, err := Plan(Request{Spec: spec("fp16", 8192), Facts: llama70B, AvailableVRAMBytes: a100})
	if err != nil {
		t.Fatal(err)
	}
	if fp16.FitsInVRAM {
		t.Errorf("70B @ fp16 needs %s and must not fit an 80 GB card",
			HumanBytes(fp16.RequiredVRAMBytes))
	}
	within(t, "70B fp16 weights", fp16.WeightsBytes, 131.5, 0.02)

	q4, err := Plan(Request{Spec: spec("q4_K_M", 8192), Facts: llama70B, AvailableVRAMBytes: a100})
	if err != nil {
		t.Fatal(err)
	}
	if !q4.FitsInVRAM {
		t.Errorf("70B @ q4_K_M needs %s and must fit an 80 GB card",
			HumanBytes(q4.RequiredVRAMBytes))
	}
	within(t, "70B q4_K_M total", q4.RequiredVRAMBytes, 50.6, 0.08)
}

func TestLlama8BOnA24GBCard(t *testing.T) {
	p, err := Plan(Request{Spec: spec("fp16", 8192), Facts: llama8B, AvailableVRAMBytes: 24 * GiB})
	if err != nil {
		t.Fatal(err)
	}
	if !p.FitsInVRAM {
		t.Errorf("8B @ fp16 8k should fit 24 GB, needs %s", HumanBytes(p.RequiredVRAMBytes))
	}
	within(t, "8B fp16 total", p.RequiredVRAMBytes, 19.1, 0.05)
	within(t, "8B kv @ 8k", p.KVCacheBytes, 1.0, 0.02)
}

// The KV cache scales linearly with concurrency, and that is the failure that
// appears under load rather than at boot: a rig sized for one sequence serves
// happily until a second client connects.
func TestKVCacheScalesLinearlyWithConcurrency(t *testing.T) {
	one, _ := Plan(Request{Spec: spec("fp16", 8192), Facts: llama70B, Concurrency: 1})
	eight, _ := Plan(Request{Spec: spec("fp16", 8192), Facts: llama70B, Concurrency: 8})

	ratio := float64(eight.KVCacheBytes) / float64(one.KVCacheBytes)
	if math.Abs(ratio-8) > 0.01 {
		t.Errorf("KV cache ratio at 8× concurrency = %.2f, want 8.0", ratio)
	}
	if eight.RequiredVRAMBytes <= one.RequiredVRAMBytes {
		t.Error("higher concurrency must increase the requirement")
	}
}

// Grouped-query attention is why a 70B's KV cache is tractable at all. If
// KVHeads were mistaken for attention heads the estimate would be 8× too
// large and every 70B would be rejected as unservable.
func TestGQAKVHeadsNotAttentionHeads(t *testing.T) {
	p, _ := Plan(Request{Spec: spec("fp16", 8192), Facts: llama70B})
	// 2 × 80 layers × 8 kv-heads × 128 dim × 8192 ctx × 2 bytes = 2.5 GiB
	within(t, "70B kv @ 8k with GQA", p.KVCacheBytes, 2.5, 0.02)

	wrong := llama70B
	wrong.KVHeads = 64 // if someone read num_attention_heads instead
	bad, _ := Plan(Request{Spec: spec("fp16", 8192), Facts: wrong})
	if bad.KVCacheBytes < p.KVCacheBytes*7 {
		t.Error("precondition: confusing the head counts should inflate the estimate ~8×")
	}
}

// §7.3: when the requested context does not fit, reduce it and say so. Never
// silently accept the requested value.
func TestContextIsReducedWithAWarningNotSilently(t *testing.T) {
	p, err := Plan(Request{
		Spec:               spec("q4_K_M", 131072),
		Facts:              llama70B,
		AvailableVRAMBytes: 48 * GiB,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.ContextLen >= 131072 {
		t.Fatalf("context should have been reduced, got %d", p.ContextLen)
	}
	if !p.FitsInVRAM {
		t.Errorf("after reduction it should fit; needs %s", HumanBytes(p.RequiredVRAMBytes))
	}
	found := false
	for _, w := range p.Warnings {
		if strings.Contains(w, "context reduced") {
			found = true
		}
	}
	if !found {
		t.Errorf("a reduced context must be warned about, got %v", p.Warnings)
	}
}

// When the weights alone exceed the card, no context reduction helps, and the
// plan must say it does not fit rather than trimming to 512 and claiming
// success.
func TestWeightsTooLargeCannotBeFixedByTrimmingContext(t *testing.T) {
	p, err := Plan(Request{
		Spec:               spec("fp16", 32768),
		Facts:              llama70B,
		AvailableVRAMBytes: 24 * GiB,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.FitsInVRAM {
		t.Error("131 GiB of weights cannot fit a 24 GB card at any context length")
	}
	if p.WeightsBytes < 24*GiB {
		t.Error("precondition: weights alone should exceed the card")
	}
}

// §7.1: unresolvable facts are a hard error. A fabricated layer count produces
// a confident VRAM figure that is wrong, and confident wrong figures get acted
// on.
func TestIncompleteFactsAreAnErrorNotAGuess(t *testing.T) {
	cases := map[string]Facts{
		"no params":  {Layers: 32, KVHeads: 8, HeadDim: 128, HiddenSize: 4096},
		"no layers":  {Params: 8, KVHeads: 8, HeadDim: 128, HiddenSize: 4096},
		"no kvheads": {Params: 8, Layers: 32, HeadDim: 128, HiddenSize: 4096},
		"no headdim": {Params: 8, Layers: 32, KVHeads: 8, HiddenSize: 4096},
		"no hidden":  {Params: 8, Layers: 32, KVHeads: 8, HeadDim: 128},
	}
	for name, f := range cases {
		if _, err := Plan(Request{Spec: spec("fp16", 4096), Facts: f}); err == nil {
			t.Errorf("%s: expected an error rather than an estimate", name)
		}
	}
}

func TestUnknownQuantizationIsRefused(t *testing.T) {
	_, err := Plan(Request{Spec: spec("q4_k_supreme", 4096), Facts: llama8B})
	if err == nil {
		t.Fatal("an unknown quantization must be refused, not assumed")
	}
	if !strings.Contains(err.Error(), "unknown quantization") {
		t.Errorf("the error should explain why: %v", err)
	}
}

func TestContextClampedToModelMaximum(t *testing.T) {
	f := llama8B
	f.MaxContextLen = 8192
	p, err := Plan(Request{Spec: spec("fp16", 32768), Facts: f})
	if err != nil {
		t.Fatal(err)
	}
	if p.ContextLen != 8192 {
		t.Errorf("context = %d, want clamping to the model maximum 8192", p.ContextLen)
	}
	if len(p.Warnings) == 0 {
		t.Error("clamping must be warned about")
	}
}

// Single-sequence sizing is the default and is the common cause of a
// load-time OOM, so it warns even when everything fits.
func TestSingleConcurrencyIsWarnedAbout(t *testing.T) {
	p, _ := Plan(Request{Spec: spec("fp16", 4096), Facts: llama8B})
	found := false
	for _, w := range p.Warnings {
		if strings.Contains(w, "concurren") {
			found = true
		}
	}
	if !found {
		t.Errorf("sizing for one sequence should warn; got %v", p.Warnings)
	}
}

// FR-SEC-29: pickle checkpoints deserialise into live objects, which is code
// execution on the host holding the operator's Hugging Face token. Rejection
// happens before an instance exists, not after.
func TestPickleWeightsAreRejectedPreSpend(t *testing.T) {
	if err := CheckWeightFormat("some/repo", FormatPickle); err == nil {
		t.Fatal("pickle weights must be refused")
	}
	if err := CheckWeightFormat("some/repo", FormatUnknown); err == nil {
		t.Fatal("an unknown weight format must be refused, not assumed safe")
	}
	for _, w := range []WeightFormat{FormatSafetensors, FormatGGUF} {
		if err := CheckWeightFormat("some/repo", w); err != nil {
			t.Errorf("%s must be accepted: %v", w, err)
		}
	}
}
