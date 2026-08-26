// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package runtime

import (
	"strings"
	"testing"
)

// The floor that let three V100 rentals through. vllm/vllm-openai:latest
// declares TORCH_CUDA_ARCH_LIST=7.5 8.0 8.6 8.9 9.0 10.0 12.0, so Volta has
// no compiled kernels in the container LARRI actually runs — but the floor
// still said 7.0, from vLLM's historical support matrix rather than from the
// image. V100 boxes are the cheapest hardware with enough total VRAM for a
// 27B model, so ranking picked them every time.
func TestVoltaIsBelowTheImagesArchList(t *testing.T) {
	r := Requirements{MinComputeCapability: 750, MinCUDA: 130, Why: "vLLM"}
	if ok, why := r.Satisfies(700); ok {
		t.Error("a V100 must not pass a floor read off an image that has no sm_70 kernels")
	} else if !strings.Contains(why, "7.0") || !strings.Contains(why, "7.5") {
		t.Errorf("the message should name both numbers: %q", why)
	}
	if ok, _ := r.Satisfies(750); !ok {
		t.Error("Turing is in the arch list and must pass")
	}
}

func TestCUDAFloorExcludesADriverTooOldForTheImage(t *testing.T) {
	r := Requirements{MinCUDA: 130, Why: "vLLM"}
	// The live host: driver 535, cuda_max_good 12.2, image CUDA_VERSION 13.0.2.
	if ok, why := r.SatisfiesCUDA(12.2); ok {
		t.Error("cuda 12.2 cannot run an image built against 13.0")
	} else if !strings.Contains(why, "12.2") || !strings.Contains(why, "13.0") {
		t.Errorf("the message should name both versions: %q", why)
	}
	if ok, _ := r.SatisfiesCUDA(13.0); !ok {
		t.Error("an exactly-matching host must pass")
	}
	// Unreported is not evidence of incompatibility; failing closed on a
	// missing field would empty the market whenever a provider stopped
	// populating it.
	if ok, _ := r.SatisfiesCUDA(0); !ok {
		t.Error("an unreported CUDA version must not exclude an offer")
	}
}
