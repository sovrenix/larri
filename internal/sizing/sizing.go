// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package sizing turns (model, quantization, context length) into a VRAM
// requirement.
//
// It is one computation consumed in three places — the search filter, the
// ranking function, and the runtime's launch flags — and it lives in one
// package precisely so that over-committing VRAM cannot be introduced in only
// one of them (invariant 5). Silently over-committing is how an operator pays
// to boot an instance that OOMs on its first request (R-08).
package sizing

import (
	"context"
	"fmt"

	"go.sovrenix.com/larri/internal/core"
)

const (
	// GiB is the unit VRAM is actually sold in.
	GiB = 1 << 30

	// DefaultSafetyFactor pads the total (§7.2).
	DefaultSafetyFactor = 1.10

	// DefaultConcurrency is the assumed number of simultaneous sequences.
	//
	// The KV cache scales linearly with it, which makes it the single most
	// common cause of an OOM that appears under load rather than at boot: a
	// rig sized for one sequence serves happily until a second client
	// connects.
	DefaultConcurrency = 1

	// DefaultKVElemBytes is fp16 KV cache. Runtimes that quantise the cache
	// override it.
	DefaultKVElemBytes = 2

	// activationFactor is an empirical multiplier over hidden × context. It
	// is small next to weights and KV, but it is not nothing at long context.
	activationFactor = 2.0

	// minOverheadBytes is the floor for CUDA context, allocator arenas, and
	// fragmentation.
	minOverheadBytes = 1.0 * GiB

	// overheadFraction is the proportional term above that floor.
	overheadFraction = 0.08
)

// Request is what to size.
type Request struct {
	Spec         core.ModelSpec
	Facts        Facts
	Concurrency  int     // 0 means DefaultConcurrency
	SafetyFactor float64 // 0 means DefaultSafetyFactor
	KVElemBytes  int     // 0 means DefaultKVElemBytes

	// AvailableVRAMBytes is the VRAM of the candidate hardware. Zero means
	// "size it, do not judge fit".
	AvailableVRAMBytes uint64
	GPUCount           int
}

// Plan estimates VRAM for a request.
//
// When the requested context does not fit, the planner reduces ContextLen to
// what does and records a warning. It never silently accepts the requested
// value, because a plan that reports success while quietly serving a shorter
// context is a plan that lies to the ranking function too.
func Plan(req Request) (core.SizingPlan, error) {
	if err := req.Facts.Validate(); err != nil {
		return core.SizingPlan{}, err
	}
	bits, err := BitsPerWeight(req.Spec.Quantization)
	if err != nil {
		return core.SizingPlan{}, err
	}
	conc := req.Concurrency
	if conc <= 0 {
		conc = DefaultConcurrency
	}
	safety := req.SafetyFactor
	if safety <= 0 {
		safety = DefaultSafetyFactor
	}
	kvElem := req.KVElemBytes
	if kvElem <= 0 {
		kvElem = DefaultKVElemBytes
	}
	gpus := req.GPUCount
	if gpus <= 0 {
		gpus = 1
	}

	ctxLen := req.Spec.ContextLen
	if ctxLen <= 0 {
		ctxLen = 4096
	}
	plan := core.SizingPlan{TensorParallelSize: gpus}

	if max := req.Facts.MaxContextLen; max > 0 && ctxLen > max {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"requested context %d exceeds the model's maximum %d; using %d",
			ctxLen, max, max))
		ctxLen = max
	}

	// Weights do not depend on context, so they are computed once.
	weights := uint64(req.Facts.Params * 1e9 * bits / 8)
	plan.WeightsBytes = weights

	fit := func(c int) (total, kv uint64) {
		kv = kvBytes(req.Facts, c, conc, kvElem)
		act := activationBytes(req.Facts, c, conc)
		over := overheadBytes(weights, kv)
		return uint64(float64(weights+kv+act+over) * safety), kv
	}

	required, kv := fit(ctxLen)

	// If a target was named and the requested context does not fit, reduce it
	// until it does rather than reporting a requirement nobody can satisfy.
	if req.AvailableVRAMBytes > 0 && required > req.AvailableVRAMBytes {
		if reduced, ok := reduceContext(req, weights, conc, kvElem, safety, ctxLen); ok {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"context reduced from %d to %d to fit %s",
				ctxLen, reduced, HumanBytes(req.AvailableVRAMBytes)))
			ctxLen = reduced
			required, kv = fit(ctxLen)
		}
	}

	plan.KVCacheBytes = kv
	plan.RequiredVRAMBytes = required
	plan.ContextLen = ctxLen

	if req.AvailableVRAMBytes > 0 {
		plan.FitsInVRAM = required <= req.AvailableVRAMBytes
		if plan.FitsInVRAM {
			plan.GPUMemUtilization = clamp(
				float64(required)/float64(req.AvailableVRAMBytes), 0.10, 0.95)
		}
	} else {
		plan.FitsInVRAM = true // nothing to fit against
		plan.GPUMemUtilization = 0.90
	}

	if conc == 1 {
		plan.Warnings = append(plan.Warnings,
			"sized for a single concurrent sequence; the KV cache scales linearly "+
				"with concurrency, so additional clients may exhaust VRAM under load")
	}
	return plan, nil
}

// kvBytes is 2 (K and V) × layers × kv-heads × head-dim × context ×
// concurrency × element size.
func kvBytes(f Facts, ctxLen, concurrency, elem int) uint64 {
	return uint64(2) *
		uint64(f.Layers) *
		uint64(f.KVHeads) *
		uint64(f.HeadDim) *
		uint64(ctxLen) *
		uint64(concurrency) *
		uint64(elem)
}

func activationBytes(f Facts, ctxLen, batch int) uint64 {
	return uint64(float64(f.HiddenSize) * float64(ctxLen) * float64(batch) *
		DefaultKVElemBytes * activationFactor)
}

func overheadBytes(weights, kv uint64) uint64 {
	proportional := float64(weights+kv) * overheadFraction
	if proportional < minOverheadBytes {
		return uint64(minOverheadBytes)
	}
	return uint64(proportional)
}

// reduceContext finds the largest power-of-two context that fits, or reports
// that no context does — which means the weights alone are too large and no
// amount of trimming helps.
func reduceContext(req Request, weights uint64, conc, kvElem int, safety float64, from int) (int, bool) {
	for c := from / 2; c >= 512; c /= 2 {
		kv := kvBytes(req.Facts, c, conc, kvElem)
		act := activationBytes(req.Facts, c, conc)
		over := overheadBytes(weights, kv)
		if uint64(float64(weights+kv+act+over)*safety) <= req.AvailableVRAMBytes {
			return c, true
		}
	}
	return 0, false
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// HumanBytes renders a byte count the way an operator thinks about VRAM.
func HumanBytes(b uint64) string {
	const unit = 1024.0
	f := float64(b)
	switch {
	case f >= unit*unit*unit:
		return fmt.Sprintf("%.1f GB", f/(unit*unit*unit))
	case f >= unit*unit:
		return fmt.Sprintf("%.0f MB", f/(unit*unit))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// PlanFor resolves facts and sizes in one call.
func PlanFor(ctx context.Context, r Resolver, req Request) (core.SizingPlan, error) {
	f, err := r.Resolve(ctx, req.Spec.Ref, req.Spec.Revision)
	if err != nil {
		return core.SizingPlan{}, err
	}
	req.Facts = f
	return Plan(req)
}
