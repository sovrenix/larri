// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package runtime

import (
	"strings"

	"go.sovrenix.com/larri/internal/core"
)

// ggufQuants are the quantisation names that only exist in GGUF-land. A model
// asked for in one of these is a llama.cpp model whatever else is true of it.
var ggufQuants = []string{
	"q2_k", "q3_k", "q4_0", "q4_1", "q4_k", "q5_0", "q5_1", "q5_k",
	"q6_k", "q8_0", "iq1_", "iq2_", "iq3_", "iq4_",
}

// Pick chooses an engine for a model (FR-RT-02, §6.3).
//
// The heuristic's job is to make the common case flagless, not to be clever —
// which is why it is a short ordered list rather than a scoring function, and
// why every branch is overridable with --runtime.
//
// The last branch is the one that earns its place: a model that does not fit in
// VRAM is not a failure, it is a llama.cpp job. vLLM must fit or die; llama.cpp
// spills layers to CPU and serves slowly. Falling back rather than refusing is
// the difference between "no offer can run this" and "this runs, at a cost".
func Pick(spec core.ModelSpec, plan core.SizingPlan, gpuCount int) core.RuntimeKind {
	ref := strings.ToLower(spec.Ref)
	quant := strings.ToLower(spec.Quantization)

	if strings.HasSuffix(ref, ".gguf") {
		return core.RuntimeLlamaCpp
	}
	for _, q := range ggufQuants {
		if strings.Contains(quant, q) {
			return core.RuntimeLlamaCpp
		}
	}
	if spec.Source == core.SourceOllamaRegistry {
		return core.RuntimeOllama
	}
	if plan.FitsInVRAM && gpuCount >= 1 {
		return core.RuntimeVLLM
	}
	return core.RuntimeLlamaCpp
}

// PickReason explains the choice in one line, so a flagless run still says why
// it is using the engine it is using.
func PickReason(spec core.ModelSpec, plan core.SizingPlan, gpuCount int) string {
	switch Pick(spec, plan, gpuCount) {
	case core.RuntimeLlamaCpp:
		if strings.HasSuffix(strings.ToLower(spec.Ref), ".gguf") {
			return "gguf weights"
		}
		for _, q := range ggufQuants {
			if strings.Contains(strings.ToLower(spec.Quantization), q) {
				return "gguf quantisation " + spec.Quantization
			}
		}
		return "does not fit in vram; offloading layers to cpu"
	case core.RuntimeOllama:
		return "ollama registry reference"
	default:
		return "fits in vram"
	}
}
