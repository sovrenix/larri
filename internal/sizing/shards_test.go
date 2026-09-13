// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sizing

import "testing"

// A layer-splitting engine takes every card. A tensor-parallel one takes only
// a degree that divides the head count — and vLLM says so at engine init, on
// a machine that has already started billing.
func TestShardsRespectsHowTheEngineSplits(t *testing.T) {
	// 40 heads, 8 KV heads: Llama-2-13B's shape and the awkward one, since
	// 40 is divisible by 8, 5, 4 and 2 but not by 6 or 7.
	f := Facts{AttentionHeads: 40, KVHeads: 8}
	for _, c := range []struct {
		name           string
		gpus           int
		tensorParallel bool
		want           int
	}{
		{"one card is one shard", 1, true, 1},
		{"layer split takes every card", 6, false, 6},
		{"exact divisor is used whole", 8, true, 8},
		{"six cards fall back to four", 6, true, 4},
		{"seven cards fall back to four", 7, true, 4},
		{"three cards fall back to two", 3, true, 2},
	} {
		if got := Shards(f, c.gpus, c.tensorParallel); got != c.want {
			t.Errorf("%s: Shards(%d) = %d, want %d", c.name, c.gpus, got, c.want)
		}
	}
}

// KV heads constrain the degree separately from attention heads: they may be
// replicated across cards, but only a whole number of times.
func TestShardsRefusesADegreeTheKVHeadsCannotTake(t *testing.T) {
	// 24 heads is divisible by 3, but 4 KV heads are neither divisible by 3
	// nor a divisor of it — so 3 is not a degree vLLM will accept.
	f := Facts{AttentionHeads: 24, KVHeads: 4}
	if got := Shards(f, 3, true); got != 2 {
		t.Errorf("Shards(3) = %d, want 2: 4 kv heads do not spread over 3 cards", got)
	}
	// 8 cards is fine: 24 divides by 8, and 8 is a whole multiple of 4.
	if got := Shards(f, 8, true); got != 8 {
		t.Errorf("Shards(8) = %d, want 8", got)
	}
}

// An unreported head count must not empty the market (§4a), and must not
// license a degree the engine may refuse either. Powers of two are the reading
// between the two.
func TestShardsFallsBackToPowersOfTwoWhenHeadsAreUnknown(t *testing.T) {
	f := Facts{KVHeads: 8} // no AttentionHeads
	for gpus, want := range map[int]int{2: 2, 3: 2, 6: 4, 8: 8, 10: 8, 16: 16} {
		if got := Shards(f, gpus, true); got != want {
			t.Errorf("Shards(%d) with unknown heads = %d, want %d", gpus, got, want)
		}
	}
}

// The cards the engine cannot reach hold no part of the model, so their VRAM
// is not available and counting it is how a host gets rented for a model it
// cannot hold.
func TestShardVRAMCountsOnlyTheCardsInUse(t *testing.T) {
	four := ShardVRAM(24, 4)
	six := ShardVRAM(24, 6)
	if four >= six {
		t.Fatal("more shards must reach more VRAM")
	}
	if want := UsableVRAM(4 * 24 * GiB); four != want {
		t.Errorf("ShardVRAM(24, 4) = %d, want the usable fraction of 96GB (%d)", four, want)
	}
}

// Every card carries its own CUDA context and allocator, so an eight-way split
// pays the overhead floor eight times even though the weights are divided.
func TestOverheadFloorIsChargedPerCard(t *testing.T) {
	// Small model, so the proportional term stays below the floor and the
	// floor is what is being measured.
	f := Facts{Params: 1, Layers: 16, KVHeads: 4, HeadDim: 64, HiddenSize: 1024}
	spec := spec("q4_K_M", 2048)
	one, err := Plan(Request{Spec: spec, Facts: f, GPUCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	eight, err := Plan(Request{Spec: spec, Facts: f, GPUCount: 8})
	if err != nil {
		t.Fatal(err)
	}
	if eight.RequiredVRAMBytes <= one.RequiredVRAMBytes {
		t.Errorf("eight cards required %s, one card %s: the per-card overhead is missing",
			HumanBytes(eight.RequiredVRAMBytes), HumanBytes(one.RequiredVRAMBytes))
	}
	if eight.TensorParallelSize != 8 {
		t.Errorf("plan carries degree %d, want 8", eight.TensorParallelSize)
	}
}
