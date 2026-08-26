// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sizing

import (
	"context"
	"fmt"
)

// Facts are the architectural parameters a VRAM estimate needs.
//
// They come from the model's own config.json (FR-RT-12, Q-06), because that is
// the same file the runtime will read. A bundled catalogue starts drifting the
// day it ships.
type Facts struct {
	Params        float64 // billions, TOTAL not active — see MoE note below
	Layers        int
	KVHeads       int // GQA: often far fewer than attention heads
	HeadDim       int
	HiddenSize    int
	MaxContextLen int

	// Ref and Revision identify what these facts describe. Revision is a
	// resolved commit rather than a branch name, so a cache hit is a fact
	// about an immutable object.
	Ref      string
	Revision string

	// MoE models carry a total parameter count far larger than the count
	// active per token. Weights must be resident regardless of which experts
	// fire, so Params is the total; ActiveParams is recorded for throughput
	// reasoning and is deliberately not used in the VRAM estimate.
	ActiveParams float64
}

// Validate reports facts that cannot support an estimate.
//
// Unresolvable facts are a hard error rather than a guess (§7.1). A fabricated
// layer count produces a confident VRAM figure that is wrong, and a confident
// wrong figure is worse than a refusal because it gets acted on.
func (f Facts) Validate() error {
	switch {
	case f.Params <= 0:
		return fmt.Errorf("sizing: %s: parameter count unknown", f.Ref)
	case f.Layers <= 0:
		return fmt.Errorf("sizing: %s: layer count unknown", f.Ref)
	case f.KVHeads <= 0:
		return fmt.Errorf("sizing: %s: KV head count unknown", f.Ref)
	case f.HeadDim <= 0:
		return fmt.Errorf("sizing: %s: head dimension unknown", f.Ref)
	case f.HiddenSize <= 0:
		return fmt.Errorf("sizing: %s: hidden size unknown", f.Ref)
	}
	return nil
}

// Resolver produces Facts for a model reference.
//
// It is an interface so that the sizing math — the part that must be right —
// is testable without a network, and so the live fetch, the cache, and an
// operator override compose rather than branch.
type Resolver interface {
	Resolve(ctx context.Context, ref, revision string) (Facts, error)
}

// StaticResolver serves preloaded facts. It backs operator overrides and every
// test in this package.
type StaticResolver map[string]Facts

// Resolve implements Resolver.
func (s StaticResolver) Resolve(_ context.Context, ref, _ string) (Facts, error) {
	f, ok := s[ref]
	if !ok {
		return Facts{}, fmt.Errorf("sizing: no facts for %q", ref)
	}
	f.Ref = ref
	return f, nil
}

// ChainResolver tries each resolver in order and returns the first success.
//
// The order is the policy from §7.1: operator override, then cache, then live
// fetch. A miss everywhere is an error, never a guess.
type ChainResolver []Resolver

// Resolve implements Resolver.
func (c ChainResolver) Resolve(ctx context.Context, ref, revision string) (Facts, error) {
	var last error
	for _, r := range c {
		f, err := r.Resolve(ctx, ref, revision)
		if err == nil {
			return f, nil
		}
		last = err
	}
	if last == nil {
		last = fmt.Errorf("sizing: no resolvers configured")
	}
	return Facts{}, fmt.Errorf("sizing: resolve facts for %q: %w", ref, last)
}

// WeightFormat is how a repository stores its weights.
type WeightFormat string

const (
	FormatSafetensors WeightFormat = "safetensors"
	FormatGGUF        WeightFormat = "gguf"
	FormatPickle      WeightFormat = "pytorch-pickle"
	FormatUnknown     WeightFormat = "unknown"
)

// ExecutesOnLoad reports whether loading this format can run arbitrary code.
//
// PyTorch pickle checkpoints deserialise into live objects, which is remote
// code execution on the machine holding the operator's Hugging Face token. It
// is a known-exploited class, and refusing it costs nothing because every
// runtime LARRI supports prefers safetensors already (FR-SEC-29).
func (w WeightFormat) ExecutesOnLoad() bool { return w == FormatPickle || w == FormatUnknown }

// CheckWeightFormat rejects formats that execute code on load, before an
// instance is created rather than after.
func CheckWeightFormat(ref string, w WeightFormat) error {
	if w.ExecutesOnLoad() {
		return fmt.Errorf("sizing: %s: weight format %s executes code on load: "+
			"safetensors required", ref, w)
	}
	return nil
}

// VariantFinder is implemented by resolvers that can look for quantised
// publications of a model.
//
// Optional, like the other capabilities: a resolver that cannot search simply
// does not implement it, and the suggestion is skipped rather than branched
// on.
type VariantFinder interface {
	FindQuantised(ctx context.Context, ref string, accept func(quant string) bool) ([]Variant, error)
}
