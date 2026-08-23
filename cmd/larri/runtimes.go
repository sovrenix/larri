// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/runtime/llamacpp"
	"go.sovrenix.com/larri/internal/runtime/ollama"
	"go.sovrenix.com/larri/internal/runtime/vllm"
	"go.sovrenix.com/larri/internal/secret"
)

// pickRuntime resolves --runtime, or chooses one from the model (§6.3).
//
// The sizing plan is not known yet at this point — it needs the model facts,
// which need a network fetch — so the automatic path decides on what the ref
// and quantisation say. That covers the branches an operator actually hits;
// the fit-based fallback to llama.cpp is applied later, where the plan exists.
func pickRuntime(name string, spec core.ModelSpec) (runtime.Runtime, error) {
	switch core.RuntimeKind(name) {
	case core.RuntimeVLLM:
		return vllm.New(), nil
	case core.RuntimeLlamaCpp:
		return newLlamaCpp(spec)
	case core.RuntimeOllama:
		return ollama.New(), nil
	case "":
	default:
		return nil, fmt.Errorf("unknown runtime %q: want vllm, llamacpp or ollama", name)
	}

	switch runtime.Pick(spec, core.SizingPlan{FitsInVRAM: true}, 1) {
	case core.RuntimeLlamaCpp:
		return newLlamaCpp(spec)
	case core.RuntimeOllama:
		return ollama.New(), nil
	default:
		return vllm.New(), nil
	}
}

// newLlamaCpp resolves which GGUF to fetch before anything is rented.
//
// A repository holds one file per quantisation and downloading the wrong one
// costs the entire transfer at rented-GPU prices, so this happens locally where
// being wrong is free.
func newLlamaCpp(spec core.ModelSpec) (runtime.Runtime, error) {
	r := llamacpp.New()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	file, err := llamacpp.ResolveGGUF(ctx, spec.Ref, spec.Quantization,
		secret.New(os.Getenv("HF_TOKEN")))
	if err != nil {
		return nil, err
	}
	fmt.Printf("  weights     %s\n", file)
	r.SetGGUF(file)
	return r, nil
}

// runtimeWhy explains the choice, so a flagless run still says why it is using
// the engine it is using.
func runtimeWhy(flag string, spec core.ModelSpec) string {
	if flag != "" {
		return "--runtime"
	}
	return runtime.PickReason(spec, core.SizingPlan{FitsInVRAM: true}, 1)
}

// securityNotes returns whatever guarantees this engine cannot give.
func securityNotes(r runtime.Runtime) []string {
	if n, ok := r.(runtime.SecurityNoter); ok {
		return n.SecurityNotes()
	}
	return nil
}
