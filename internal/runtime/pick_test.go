// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package runtime

import (
	"testing"

	"go.sovrenix.com/larri/internal/core"
)

func TestPickFollowsTheHeuristic(t *testing.T) {
	fits := core.SizingPlan{FitsInVRAM: true}
	doesNot := core.SizingPlan{FitsInVRAM: false}

	cases := []struct {
		name string
		spec core.ModelSpec
		plan core.SizingPlan
		gpus int
		want core.RuntimeKind
	}{
		{"a gguf ref is llama.cpp whatever else is true",
			core.ModelSpec{Ref: "org/model/file.Q4_K_M.gguf"}, fits, 1, core.RuntimeLlamaCpp},
		{"a gguf quantisation is llama.cpp",
			core.ModelSpec{Ref: "org/model", Quantization: "q4_K_M"}, fits, 1, core.RuntimeLlamaCpp},
		{"an ollama reference is ollama",
			core.ModelSpec{Ref: "llama3.1:70b", Source: core.SourceOllamaRegistry}, fits, 1, core.RuntimeOllama},
		{"safetensors that fit are vllm",
			core.ModelSpec{Ref: "Qwen/Qwen3", Quantization: "fp16"}, fits, 1, core.RuntimeVLLM},
		// The branch that matters: not fitting is a reason to change engine,
		// not a reason to refuse. llama.cpp offloads to CPU and serves slowly,
		// which beats telling the operator no offer can run their model.
		{"safetensors that do not fit fall back to llama.cpp",
			core.ModelSpec{Ref: "Qwen/Qwen3", Quantization: "fp16"}, doesNot, 1, core.RuntimeLlamaCpp},
		{"no gpu falls back to llama.cpp",
			core.ModelSpec{Ref: "Qwen/Qwen3", Quantization: "fp16"}, fits, 0, core.RuntimeLlamaCpp},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Pick(c.spec, c.plan, c.gpus); got != c.want {
				t.Errorf("Pick = %s, want %s", got, c.want)
			}
			if PickReason(c.spec, c.plan, c.gpus) == "" {
				t.Error("a flagless choice must still say why")
			}
		})
	}
}

// The ordering bug this pins: once an Ollama tag has been inspected, its real
// quantisation is recorded — and every Ollama model is GGUF-quantised, so a
// quantisation test that ran first would route "qwen2.5:1.5b" to llama.cpp,
// which would then try to fetch a registry blob from Hugging Face.
func TestOllamaSourceOutranksItsGGUFQuantisation(t *testing.T) {
	spec := core.ModelSpec{
		Ref: "qwen2.5:1.5b", Source: core.SourceOllamaRegistry,
		Quantization: "Q4_K_M", // read from the model's own header
	}
	if got := Pick(spec, core.SizingPlan{FitsInVRAM: true}, 1); got != core.RuntimeOllama {
		t.Errorf("Pick = %s, want ollama", got)
	}
	if r := PickReason(spec, core.SizingPlan{FitsInVRAM: true}, 1); r != "ollama registry reference" {
		t.Errorf("reason = %q", r)
	}
}
