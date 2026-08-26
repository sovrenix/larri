// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"go.sovrenix.com/larri/internal/config"
	"os"
	"strings"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/provider"
	_ "go.sovrenix.com/larri/internal/provider/runpod"
	_ "go.sovrenix.com/larri/internal/provider/vastai"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/runtime/llamacpp"
	"go.sovrenix.com/larri/internal/runtime/ollama"
	"go.sovrenix.com/larri/internal/runtime/vllm"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/sizing"
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

// isOllamaRef distinguishes "llama3.1:70b" from "org/repo".
//
// The shapes are unambiguous — a tag carries a colon and no slash — so making
// the operator also pass --runtime would be asking them to repeat themselves.
// One definition, because `up` and `offers` disagreeing about what a reference
// means is how a preview ends up ranking a different market than the one that
// gets rented from.
func isOllamaRef(ref string) bool {
	return strings.Contains(ref, ":") && !strings.Contains(ref, "/")
}

// prepareSpec fills in what only the model itself can say.
//
// For an Ollama tag that is the quantisation: the tag ships one, the operator
// does not choose it, and sizing against whatever they typed (or the fp16
// default) would be sizing against fiction. The resolver comes back too,
// because Hugging Face cannot answer for a reference that is not a Hugging
// Face repository.
func prepareSpec(ctx context.Context, spec *core.ModelSpec) (sizing.Resolver, error) {
	if spec.Source != core.SourceOllamaRegistry {
		return sizing.NewHFResolver(secret.New(os.Getenv("HF_TOKEN"))), nil
	}
	info, err := ollama.Inspect(ctx, spec.Ref)
	if err != nil {
		return nil, err
	}
	spec.Quantization = info.Quantization
	fmt.Printf("  weights     %s %s, %s\n", spec.Ref, info.Quantization,
		sizing.HumanBytes(uint64(info.WeightBytes)))
	return sizing.StaticResolver{spec.Ref: info.Facts()}, nil
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

// openProvider resolves the provider to use.
//
// One call site for what was six copies of `vastai.New(os.Getenv(...))`. The
// duplication was harmless while there was one provider and would have been
// six edits and a missed one the moment there were two.
// openProvider resolves which provider to rent from.
//
// An explicit flag wins. Otherwise the configured order decides, and only a
// machine with no usable provider at all is an error. Refusing to choose
// between two configured providers made --provider mandatory the moment a
// second one was compiled in, which is a flag the operator has to get right
// before anything works and which the configuration already answers.
func openProvider(name string) (provider.Provider, error) {
	if name != "" {
		return provider.Open(name)
	}
	if d, err := provider.Default(); err == nil {
		return provider.Open(d)
	}
	// Several are available: take the first configured one that opens. A
	// provider fails to open when its credential is absent, so this also
	// skips the ones the operator has not set up.
	var configured []string
	if res, err := config.Resolve(config.Request{}); err == nil && res != nil {
		configured = res.Config.Providers
	}
	var last error
	for _, n := range configured {
		p, err := provider.Open(n)
		if err == nil {
			return p, nil
		}
		last = err
	}
	for _, n := range provider.Names() {
		p, err := provider.Open(n)
		if err == nil {
			return p, nil
		}
		last = err
	}
	if last == nil {
		last = fmt.Errorf("no providers are compiled in")
	}
	return nil, last
}
