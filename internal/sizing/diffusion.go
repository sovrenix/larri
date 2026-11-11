// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sizing

import (
	"fmt"

	"go.sovrenix.com/larri/internal/core"
)

// Diffusion sizing lives here rather than beside the ComfyUI adapter for the
// reason invariant 5 gives: (payload, shape) -> required VRAM is one
// computation consumed by the search filter, the ranking function, and the
// launch flags, and splitting it across packages is how those three quietly
// stop agreeing.
//
// The arithmetic is not the transformer arithmetic and does not reuse it. A
// diffusion graph has no KV cache and no context length; what it has is a set
// of resident modules and a latent whose area drives every intermediate
// tensor. Plan() would need every one of its terms replaced, which is a
// different function wearing the same name.

const (
	// diffusionWorkspaceBytes is the fixed cost of having a diffusion graph
	// loaded at all: the CUDA context, cuDNN and attention workspaces, and
	// the allocator arenas that do not shrink between nodes.
	//
	// Separate from the per-pixel term because the two do not scale together.
	// A 512x512 SD1.5 render and a 1024x1024 SDXL render differ four-fold in
	// area and nothing like four-fold in total, which is the signature of a
	// fixed component large enough to dominate the small case.
	diffusionWorkspaceBytes = 1.0 * GiB

	// diffusionBytesPerPixel is the marginal cost of output area.
	//
	// It is an empirical fit, and saying so is more useful than implying a
	// derivation. Two reference points set it: SD1.5 at 512x512 runs in about
	// 1.8 GB above its weights, and SDXL at 1024x1024 in about 4.1 GB above
	// its own — which a 1 GB fixed term and roughly 3 KB per pixel reproduce
	// to within the safety factor. The peak is the VAE decode at the end of
	// the graph rather than any sampling step, so this is sized for the
	// moment the run is largest rather than for its average.
	diffusionBytesPerPixel = 3072.0

	// DefaultPixels is the area assumed when a graph names no latent size.
	// 1024x1024 is SDXL's native resolution and the shape most stock graphs
	// carry; assuming something smaller would size a rig that cannot do the
	// obvious next thing the operator tries.
	DefaultPixels = 1024 * 1024
)

// DiffusionRequest is what to size for an image-generation graph.
type DiffusionRequest struct {
	// WeightBytes is the resident model footprint — every file the graph
	// loads, in the precision it is published in.
	WeightBytes uint64

	// Pixels is width x height x batch of the largest latent in the graph.
	// Zero means DefaultPixels.
	Pixels int

	// AvailableVRAMBytes is the VRAM of the candidate hardware. Zero means
	// "size it, do not judge fit".
	AvailableVRAMBytes uint64

	GPUCount     int
	SafetyFactor float64 // 0 means DefaultSafetyFactor
}

// PlanDiffusion estimates VRAM for a ComfyUI graph.
//
// It reports a *target* rather than a hard floor, and the difference from the
// vLLM path is worth stating plainly because it changes what a near-miss
// costs. vLLM must fit or it dies at load. ComfyUI does not: when VRAM runs
// short it moves modules back to host RAM between nodes and carries on, so an
// undersized card produces the right image slowly instead of no image at all.
//
// That is why FitsInVRAM being false is not fatal *to the engine*. It is still
// fatal to the rental, and the ComfyUI claw enforces the target as a hard
// floor for that reason: a rig that thrashes its weights across PCIe for every
// sampling step is one the operator is paying full price for and getting a
// fraction of, and §4b prices a slow rig as an expensive one. "No offers" is
// free and recoverable; a billing rig producing an image an hour is not.
//
// So the offload path is a property of the engine that LARRI declines to rely
// on, not a selection outcome an operator can reach. Said plainly because the
// wording here previously implied the opposite, and a reader looking for the
// warning that goes with it would not have found one: the claw passes no
// AvailableVRAMBytes, so the branch below never runs for a ComfyUI graph, and
// an undersized offer is rejected by ranking with the shortfall named rather
// than rented with a caveat.
//
// Multi-GPU does not help and is deliberately not counted. ComfyUI executes a
// graph on one device; a second card adds VRAM that nothing in the graph can
// reach, so summing across cards here would select hardware whose headroom is
// arithmetic rather than real.
func PlanDiffusion(req DiffusionRequest) (core.SizingPlan, error) {
	if req.WeightBytes == 0 {
		return core.SizingPlan{}, fmt.Errorf("sizing: diffusion: no model weights to size")
	}
	pixels := req.Pixels
	if pixels <= 0 {
		pixels = DefaultPixels
	}
	safety := req.SafetyFactor
	if safety <= 0 {
		safety = DefaultSafetyFactor
	}

	activations := uint64(diffusionWorkspaceBytes + diffusionBytesPerPixel*float64(pixels))
	// One GPU's worth of overhead, whatever the host has. The floor scales
	// per card because each carries its own CUDA context and allocator, and a
	// diffusion graph only ever occupies one of them.
	over := overheadBytes(req.WeightBytes, activations, 1)
	required := uint64(float64(req.WeightBytes+activations+over) * safety)

	plan := core.SizingPlan{
		WeightsBytes: req.WeightBytes,
		// Recorded under the KV-cache field because SizingPlan is the
		// persisted shape every surface already reads, and the field means
		// "the part that scales with the shape of the work" in both engines.
		// Naming it for diffusion would fork the type; leaving it empty would
		// make the status output claim a 7 GB requirement made of 7 GB of
		// weights and nothing else.
		KVCacheBytes:       activations,
		RequiredVRAMBytes:  required,
		TensorParallelSize: 1,
	}
	if req.AvailableVRAMBytes > 0 {
		plan.FitsInVRAM = required <= req.AvailableVRAMBytes
		if plan.FitsInVRAM {
			plan.GPUMemUtilization = clamp(
				float64(required)/float64(req.AvailableVRAMBytes), 0.10, 0.95)
		} else {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"needs %s and the card has %s; comfyui will offload to host ram and render slowly",
				HumanBytes(required), HumanBytes(req.AvailableVRAMBytes)))
		}
	} else {
		plan.FitsInVRAM = true
		plan.GPUMemUtilization = 0.90
	}
	if req.GPUCount > 1 {
		plan.Warnings = append(plan.Warnings,
			"comfyui executes a graph on one gpu; the additional cards are idle")
	}
	return plan, nil
}

// DiffusionHostRAMBytes is the host memory a graph needs to be comfortable.
//
// ComfyUI's answer to insufficient VRAM is host RAM, so the two are not
// independent: a card that cannot hold the bundle needs somewhere to put the
// modules it is not currently running, and a box with too little RAM turns
// that into swap. Twice the bundle plus a floor covers the checkpoint being
// resident in both places during a load, which is the transient peak.
func DiffusionHostRAMBytes(weightBytes uint64) uint64 {
	const floor = 16 * GiB
	if n := weightBytes * 2; n > floor {
		return n
	}
	return floor
}
