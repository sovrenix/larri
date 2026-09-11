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
	// fragmentation. It is charged **per card**: each GPU in a shard set
	// carries its own context and its own allocator, so an eight-way split
	// pays this eight times over even though the weights are divided.
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

	// GPUCount is how many cards the model will actually be placed on, which
	// is Shards(...) rather than the number the host advertises. It raises
	// the overhead floor — one CUDA context per card — and is carried into
	// the plan as the tensor-parallel degree.
	GPUCount int

	// Shards answers, for a host with that many cards, how many the engine
	// can reach. Only the shortfall report needs it, because that report
	// reasons about offers rather than about one chosen machine. Nil means
	// every card counts, which is what a layer-splitting engine does anyway.
	Shards func(gpus int) int

	// WeightBytes is the measured size of the weights this rig will load,
	// when the repository published it. Zero means it did not, and the
	// estimate from Params and the quantisation stands in.
	//
	// A measurement beats that estimate outright, because the estimate is
	// two approximations multiplied: a parameter count rounded to a
	// marketing figure, and a table of average bits per weight that no
	// publisher is obliged to agree with. Unsloth's UD-IQ1_S is 3.22 bits
	// per weight where the name says 1.56 — sizing it from the table
	// under-commits by half, and an under-committed plan OOMs after the rig
	// is paid for. It also means a quantisation the table has never heard of
	// still sizes, which matters because the naming schemes keep arriving.
	//
	// It belongs here rather than on Facts because it is a fact about a
	// *file*, not about a model: one repository at one revision publishes a
	// dozen quantisations, and the facts cache is keyed by revision alone.
	// Caching this there would hand the next quantisation the last one's
	// size.
	WeightBytes uint64
}

// shards is Request.Shards with the nil case filled in.
func (r Request) shards(gpus int) int {
	if gpus < 1 {
		return 1
	}
	if r.Shards == nil {
		return gpus
	}
	return r.Shards(gpus)
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
	// A measured size skips the estimate entirely, including its demand that
	// the quantisation be one the table knows. Nothing downstream needs the
	// bits figure once the bytes are known.
	var bits float64
	if req.WeightBytes == 0 {
		var err error
		if bits, err = BitsPerWeight(req.Spec.Quantization); err != nil {
			return core.SizingPlan{}, err
		}
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
	weights := req.WeightBytes
	if weights == 0 {
		weights = uint64(req.Facts.Params * 1e9 * bits / 8)
	}
	plan.WeightsBytes = weights

	fit := func(c int) (total, kv uint64) {
		kv = kvBytes(req.Facts, c, conc, kvElem)
		act := activationBytes(req.Facts, c, conc)
		over := overheadBytes(weights, kv, gpus)
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
				float64(required)/float64(req.AvailableVRAMBytes), 0.10, MaxGPUUtilisation)
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

func overheadBytes(weights, kv uint64, gpus int) uint64 {
	if gpus < 1 {
		gpus = 1
	}
	floor := minOverheadBytes * float64(gpus)
	proportional := float64(weights+kv) * overheadFraction
	if proportional < floor {
		return uint64(floor)
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
		over := overheadBytes(weights, kv, req.GPUCount)
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
// MaxGPUUtilisation is the largest fraction of a card vLLM is asked to take.
//
// It is not 1.0 and cannot be: the driver, the CUDA context and the engine's
// own allocator live in the same memory, so a fraction above this fails at
// init rather than merely running tight.
//
// It has to be shared with the offer filter, which is the mistake that made
// it worth naming. Selection compared the requirement against a card's *total*
// VRAM while the launch handed vLLM 0.90 of it, so an 11.2 GB model "fitted" a
// 12 GB card and was then given 10.8 GB. The arithmetic was lost before the
// rig was rented.
const MaxGPUUtilisation = 0.95

// UsableVRAM is how much of a card a runtime can actually allocate.
func UsableVRAM(totalBytes uint64) uint64 {
	return uint64(float64(totalBytes) * MaxGPUUtilisation)
}

func PlanFor(ctx context.Context, r Resolver, req Request) (core.SizingPlan, error) {
	f, err := r.Resolve(ctx, req.Spec.Ref, req.Spec.Revision)
	if err != nil {
		return core.SizingPlan{}, err
	}
	req.Facts = f
	return Plan(req)
}
