// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sizing

import (
	"fmt"

	"go.sovrenix.com/larri/internal/core"
)

// Speech sizing lives here for the reason invariant 5 gives, and is a third
// arithmetic rather than a variant of either of the other two.
//
// A speech model has no context length the operator chooses. Whisper's encoder
// sees a fixed thirty-second window whatever the file is, and its decoder emits
// at most a few hundred tokens per window, so the transformer path's central
// term — a KV cache that grows with the context asked for — has no meaning
// here. What the working set actually scales with is how many windows are in
// flight at once and how wide the beam is.

const (
	// speechWorkspaceBytes is the cost of having the model loaded at all:
	// the CUDA context, cuDNN's convolution workspaces for the encoder's
	// front end, and allocator arenas that do not shrink between requests.
	speechWorkspaceBytes = 0.8 * GiB

	// speechEncoderFraction is one in-flight thirty-second window's encoder
	// activations, as a fraction of the model at float16.
	//
	// A fraction rather than a constant because the term scales with the
	// model's width, and the float16 size is the closest proxy for width that
	// a caller reliably has. An empirical fit, and saying so is more useful
	// than implying a derivation: faster-whisper large-v3 at float16 sits
	// around 4.7 GB resident at beam 5, which this reproduces to within the
	// safety factor.
	speechEncoderFraction = 0.30

	// speechBeamFraction is the decoder cache per beam per in-flight window,
	// again against the float16 size. Small, because the sequence is short —
	// this is what makes speech cheap next to a chat model of the same width.
	speechBeamFraction = 0.02

	// DefaultBeamSize is what the transcription servers default to, and what
	// to assume when a job does not say.
	DefaultBeamSize = 5
)

// SpeechRequest is what to size for a speech-to-text server.
type SpeechRequest struct {
	// WeightBytes is the resident model footprint in the precision it is
	// published in.
	WeightBytes uint64

	// FP16WeightBytes is the same model at float16, which is what the working
	// set scales with. Zero means WeightBytes.
	//
	// Two numbers rather than one because quantisation shrinks one of them
	// and not the other. CTranslate2 stores int8 weights and still computes
	// in float16, so an int8 model halves what it occupies at rest and leaves
	// its working set exactly where it was. Scaling activations off the
	// quantised number is how an int8 rig gets chosen a card too small and
	// then dies at the first long file.
	FP16WeightBytes uint64

	// Concurrency is how many transcriptions may be in flight at once. Zero
	// means one, which is what a single operator with a single client does.
	Concurrency int

	// BeamSize is the decoder's beam width. Zero means DefaultBeamSize.
	BeamSize int

	// AvailableVRAMBytes is the VRAM of the candidate hardware. Zero means
	// "size it, do not judge fit".
	AvailableVRAMBytes uint64

	GPUCount     int
	SafetyFactor float64 // 0 means DefaultSafetyFactor
}

// PlanSpeech estimates VRAM for a speech-to-text server.
//
// Unlike the diffusion path, a shortfall here is fatal rather than slow.
// ComfyUI answers insufficient VRAM by moving modules back to host RAM and
// carrying on; CTranslate2 does not offload, so a card that cannot hold the
// model fails at load or at the first request. FitsInVRAM being false is
// therefore a reason not to rent, and the warning says so in those terms
// rather than in the diffusion path's "slowly".
//
// Multi-GPU is not counted. A single transcription server pins one device, and
// summing across cards would select hardware whose headroom is arithmetic.
func PlanSpeech(req SpeechRequest) (core.SizingPlan, error) {
	if req.WeightBytes == 0 {
		return core.SizingPlan{}, fmt.Errorf("sizing: speech: no model weights to size")
	}
	fp16 := req.FP16WeightBytes
	if fp16 == 0 {
		fp16 = req.WeightBytes
	}
	streams := req.Concurrency
	if streams <= 0 {
		streams = 1
	}
	beam := req.BeamSize
	if beam <= 0 {
		beam = DefaultBeamSize
	}
	safety := req.SafetyFactor
	if safety <= 0 {
		safety = DefaultSafetyFactor
	}

	perStream := float64(fp16) * (speechEncoderFraction + speechBeamFraction*float64(beam))
	working := uint64(speechWorkspaceBytes + perStream*float64(streams))
	over := overheadBytes(req.WeightBytes, working, 1)
	required := uint64(float64(req.WeightBytes+working+over) * safety)

	plan := core.SizingPlan{
		WeightsBytes: req.WeightBytes,
		// Under the KV-cache field for the reason the diffusion path uses it:
		// SizingPlan is the persisted shape every surface already reads, and
		// the field means "the part that scales with the shape of the work".
		// Leaving it empty would have status report a requirement made of
		// weights and nothing else.
		KVCacheBytes:       working,
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
				"needs %s and the card has %s; ctranslate2 does not offload, so it fails at load",
				HumanBytes(required), HumanBytes(req.AvailableVRAMBytes)))
		}
	} else {
		plan.FitsInVRAM = true
		plan.GPUMemUtilization = 0.90
	}
	if req.GPUCount > 1 {
		plan.Warnings = append(plan.Warnings,
			"a transcription server pins one gpu; the additional cards are idle")
	}
	return plan, nil
}
