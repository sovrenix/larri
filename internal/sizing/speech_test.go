// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sizing

import (
	"strings"
	"testing"
)

// large-v3 in CTranslate2, the model this path exists to size.
const (
	whisperLargeFP16 = 3100 * 1024 * 1024 // ~3.1 GB
	whisperLargeInt8 = 1600 * 1024 * 1024 // ~1.6 GB
)

// The number decides what card gets rented, so it is worth pinning to the
// observed one. faster-whisper large-v3 at float16 and beam 5 sits around
// 4.7 GB resident; the plan must cover that and must not ask for a card twice
// the size, which would price the claw out of its own market.
func TestLargeV3AtFloat16LandsWhereTheServerActuallySits(t *testing.T) {
	plan, err := PlanSpeech(SpeechRequest{WeightBytes: whisperLargeFP16})
	if err != nil {
		t.Fatal(err)
	}
	got := float64(plan.RequiredVRAMBytes) / GiB
	if got < 4.7 || got > 8 {
		t.Errorf("required = %.1f GiB, want between the observed 4.7 and a sane ceiling of 8: %s",
			got, HumanBytes(plan.RequiredVRAMBytes))
	}
	if plan.KVCacheBytes == 0 {
		t.Error("the working set is zero, so status would report a requirement made of weights alone")
	}
	if plan.TensorParallelSize != 1 {
		t.Errorf("tensor parallel = %d: a transcription server pins one device",
			plan.TensorParallelSize)
	}
}

// The whole reason SpeechRequest carries two weight figures. CTranslate2 keeps
// int8 weights and still computes in float16, so quantising halves what the
// model occupies at rest and leaves the working set alone. Sizing the working
// set off the quantised number is how an int8 rig gets a card too small.
func TestQuantisingShrinksTheWeightsAndNotTheWorkingSet(t *testing.T) {
	fp16, err := PlanSpeech(SpeechRequest{WeightBytes: whisperLargeFP16})
	if err != nil {
		t.Fatal(err)
	}
	int8, err := PlanSpeech(SpeechRequest{
		WeightBytes: whisperLargeInt8, FP16WeightBytes: whisperLargeFP16,
	})
	if err != nil {
		t.Fatal(err)
	}
	if int8.KVCacheBytes != fp16.KVCacheBytes {
		t.Errorf("working set moved with quantisation: %s int8 vs %s fp16",
			HumanBytes(int8.KVCacheBytes), HumanBytes(fp16.KVCacheBytes))
	}
	if int8.RequiredVRAMBytes >= fp16.RequiredVRAMBytes {
		t.Error("quantising saved nothing at all")
	}
	// The saving is the weight difference and no more. Anyone expecting the
	// requirement to halve is going to rent the wrong card.
	saved := fp16.RequiredVRAMBytes - int8.RequiredVRAMBytes
	if saved > fp16.RequiredVRAMBytes/2 {
		t.Errorf("quantising cut the requirement by %s, which is more than the weights it removed",
			HumanBytes(saved))
	}

	// Omitting FP16WeightBytes is the mistake the field exists to prevent: it
	// sizes the working set off the quantised figure and asks for less.
	naive, err := PlanSpeech(SpeechRequest{WeightBytes: whisperLargeInt8})
	if err != nil {
		t.Fatal(err)
	}
	if naive.RequiredVRAMBytes >= int8.RequiredVRAMBytes {
		t.Error("the fp16 figure changed nothing, so the field is not doing its job")
	}
}

func TestConcurrencyAndBeamScaleTheWorkingSet(t *testing.T) {
	one, _ := PlanSpeech(SpeechRequest{WeightBytes: whisperLargeFP16})
	four, _ := PlanSpeech(SpeechRequest{WeightBytes: whisperLargeFP16, Concurrency: 4})
	if four.KVCacheBytes <= one.KVCacheBytes {
		t.Error("four in-flight windows cost no more than one")
	}

	greedy, _ := PlanSpeech(SpeechRequest{WeightBytes: whisperLargeFP16, BeamSize: 1})
	if greedy.KVCacheBytes >= one.KVCacheBytes {
		t.Error("a beam of 1 costs at least as much as the default of 5")
	}
	// Zero must mean the default rather than a free ride.
	def, _ := PlanSpeech(SpeechRequest{WeightBytes: whisperLargeFP16, BeamSize: 0})
	if def.RequiredVRAMBytes != one.RequiredVRAMBytes {
		t.Error("an unset beam size was not treated as the default")
	}
}

// Unlike the diffusion path, a shortfall here is a reason not to rent:
// CTranslate2 does not offload, so the server dies rather than slows.
func TestAShortfallSaysItIsFatalRatherThanSlow(t *testing.T) {
	plan, err := PlanSpeech(SpeechRequest{
		WeightBytes: whisperLargeFP16, AvailableVRAMBytes: 2 * GiB,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.FitsInVRAM {
		t.Fatal("large-v3 was reported to fit in 2 GiB")
	}
	if len(plan.Warnings) == 0 {
		t.Fatal("a shortfall produced no warning")
	}
	if !contains(plan.Warnings, "fails at load") {
		t.Errorf("the warning does not say the shortfall is fatal: %q", plan.Warnings)
	}
}

func TestSizingRefusesAModelWithNoWeights(t *testing.T) {
	if _, err := PlanSpeech(SpeechRequest{}); err == nil {
		t.Fatal("a model with no weights was sized successfully")
	}
}

func TestExtraCardsAreCalledIdleRatherThanCounted(t *testing.T) {
	plan, err := PlanSpeech(SpeechRequest{WeightBytes: whisperLargeFP16, GPUCount: 4})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(plan.Warnings, "one gpu") {
		t.Errorf("four cards drew no warning: %q", plan.Warnings)
	}
}

func contains(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}
