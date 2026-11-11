// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sizing

import (
	"strings"
	"testing"
)

const (
	sdxlWeights = 6_940_000_000  // sd_xl_base_1.0 fp16
	fluxWeights = 23_800_000_000 // flux1-dev plus its text encoders
)

// The two reference configurations the coefficients were fitted to. If a
// change to the arithmetic moves these outside the bands below, the change has
// moved the estimate away from the hardware it was calibrated against.
func TestDiffusionSizingMatchesItsReferencePoints(t *testing.T) {
	for _, tc := range []struct {
		name              string
		weights           uint64
		pixels            int
		wantGiBLo, wantHi float64
	}{
		{"sdxl 1024x1024", sdxlWeights, 1024 * 1024, 11, 14},
		{"sd1.5 512x512", 2_130_000_000, 512 * 512, 4, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := PlanDiffusion(DiffusionRequest{
				WeightBytes: tc.weights, Pixels: tc.pixels,
			})
			if err != nil {
				t.Fatal(err)
			}
			got := float64(plan.RequiredVRAMBytes) / GiB
			if got < tc.wantGiBLo || got > tc.wantHi {
				t.Errorf("required = %.1f GiB, want between %.0f and %.0f",
					got, tc.wantGiBLo, tc.wantHi)
			}
		})
	}
}

// Area is the term that scales, and it must actually scale: a planner that
// ignored resolution would size a 2048x2048 render like a thumbnail and OOM
// on the VAE decode.
func TestLargerLatentsNeedMoreVRAM(t *testing.T) {
	small, err := PlanDiffusion(DiffusionRequest{WeightBytes: sdxlWeights, Pixels: 512 * 512})
	if err != nil {
		t.Fatal(err)
	}
	large, err := PlanDiffusion(DiffusionRequest{WeightBytes: sdxlWeights, Pixels: 2048 * 2048})
	if err != nil {
		t.Fatal(err)
	}
	if large.RequiredVRAMBytes <= small.RequiredVRAMBytes {
		t.Fatalf("2048x2048 (%d) did not exceed 512x512 (%d)",
			large.RequiredVRAMBytes, small.RequiredVRAMBytes)
	}
	// And the growth is in the activation term, not the weights.
	if large.WeightsBytes != small.WeightsBytes {
		t.Error("weights changed with resolution")
	}
	if large.KVCacheBytes <= small.KVCacheBytes {
		t.Error("the activation term did not grow with area")
	}
}

// Batch size multiplies the latent, which is why it is folded into Pixels
// rather than tracked separately. Four images at once is four times the work.
func TestBatchCountsTowardArea(t *testing.T) {
	one, _ := PlanDiffusion(DiffusionRequest{WeightBytes: sdxlWeights, Pixels: 1024 * 1024})
	four, _ := PlanDiffusion(DiffusionRequest{WeightBytes: sdxlWeights, Pixels: 1024 * 1024 * 4})
	if four.RequiredVRAMBytes <= one.RequiredVRAMBytes {
		t.Error("a batch of four was sized like a batch of one")
	}
}

// Under-fitting ComfyUI is not fatal — it offloads and renders slowly — but it
// is a bad rental, so the warning has to say what the operator is buying.
func TestAShortCardWarnsRatherThanFailing(t *testing.T) {
	plan, err := PlanDiffusion(DiffusionRequest{
		WeightBytes:        fluxWeights,
		Pixels:             1024 * 1024,
		AvailableVRAMBytes: 8 * GiB,
	})
	if err != nil {
		t.Fatalf("a card too small must not be an error: %v", err)
	}
	if plan.FitsInVRAM {
		t.Error("8 GiB was reported as holding a 23.8 GB bundle")
	}
	if len(plan.Warnings) == 0 {
		t.Fatal("no warning for a card that cannot hold the bundle")
	}
	if !strings.Contains(strings.Join(plan.Warnings, " "), "offload") {
		t.Errorf("the warning does not say what happens: %v", plan.Warnings)
	}
}

func TestAmpleVRAMFits(t *testing.T) {
	plan, err := PlanDiffusion(DiffusionRequest{
		WeightBytes: sdxlWeights, Pixels: 1024 * 1024,
		AvailableVRAMBytes: 48 * GiB,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.FitsInVRAM {
		t.Error("48 GiB did not hold an SDXL bundle")
	}
	if plan.GPUMemUtilization <= 0 || plan.GPUMemUtilization > 0.95 {
		t.Errorf("utilisation = %v, out of range", plan.GPUMemUtilization)
	}
}

// ComfyUI runs a graph on one device, so a second card is VRAM nothing can
// reach. Saying so is what stops the operator paying for headroom that is
// arithmetic rather than real.
func TestExtraGPUsAreReportedAsIdle(t *testing.T) {
	plan, err := PlanDiffusion(DiffusionRequest{
		WeightBytes: sdxlWeights, Pixels: 1024 * 1024, GPUCount: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.TensorParallelSize != 1 {
		t.Errorf("tensor parallel = %d, want 1", plan.TensorParallelSize)
	}
	if !strings.Contains(strings.Join(plan.Warnings, " "), "idle") {
		t.Errorf("no warning that the extra cards are unused: %v", plan.Warnings)
	}
}

func TestNoWeightsIsAnError(t *testing.T) {
	if _, err := PlanDiffusion(DiffusionRequest{Pixels: 1024 * 1024}); err == nil {
		t.Error("sized a bundle with no weights in it")
	}
}

// A graph that names no latent still has to be sized for something, and the
// something must be SDXL's native resolution rather than nothing.
func TestMissingLatentFallsBackToTheDefault(t *testing.T) {
	withDefault, _ := PlanDiffusion(DiffusionRequest{WeightBytes: sdxlWeights})
	explicit, _ := PlanDiffusion(DiffusionRequest{
		WeightBytes: sdxlWeights, Pixels: DefaultPixels})
	if withDefault.RequiredVRAMBytes != explicit.RequiredVRAMBytes {
		t.Error("an unstated latent did not fall back to the default")
	}
}

// Host RAM is where ComfyUI puts what will not fit, so it is a floor derived
// from the bundle rather than a constant.
func TestHostRAMScalesWithTheBundleAndHasAFloor(t *testing.T) {
	if got := DiffusionHostRAMBytes(1 * GiB); got != 16*GiB {
		t.Errorf("small bundle = %d, want the 16 GiB floor", got)
	}
	if got := DiffusionHostRAMBytes(40 * GiB); got != 80*GiB {
		t.Errorf("large bundle = %d, want twice the bundle", got)
	}
}
