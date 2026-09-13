// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sizing

// Shards is how many of a host's GPUs a runtime can actually put the model on.
//
// It exists because "the box has eight cards" and "the engine can use eight
// cards for this model" are different claims, and only the second one is worth
// paying for. Selection sizes against the aggregate VRAM of the cards a shard
// count can reach, so a count that overstates it rents hardware that OOMs and
// a count that understates it rejects hosts that would have worked.
//
// Two engines, two rules:
//
//   - **Layer split** (llama.cpp, Ollama) divides the model by layer and needs
//     no arithmetic relationship at all. Every card is usable.
//   - **Tensor parallelism** (vLLM) divides each layer's attention heads, so
//     the degree must divide the head count. vLLM rejects a degree that does
//     not at engine init, which is after the rental — a 6-card box serving a
//     40-head model shards across 4 and leaves two idle, and that is still a
//     rig, whereas a refused launch is a bill.
func Shards(f Facts, gpus int, tensorParallel bool) int {
	if gpus <= 1 {
		return 1
	}
	if !tensorParallel {
		return gpus
	}
	// An unreported head count proves nothing (§4a), so it must not empty the
	// market — but it also cannot license a degree the engine may refuse.
	// Powers of two are the safe reading between the two: published head
	// counts are 16, 24, 28, 32, 40, 64, 96, 128, and a power of two divides
	// all but the odd multiples.
	if f.AttentionHeads <= 0 {
		return largestPowerOfTwo(gpus)
	}
	for n := gpus; n > 1; n-- {
		if shardable(f, n) {
			return n
		}
	}
	return 1
}

// shardable reports whether a tensor-parallel degree divides the model.
//
// Both head counts constrain it. Attention heads must divide evenly. KV heads
// are fewer under GQA and may instead be *replicated* across cards, which the
// engine allows only when the degree is a whole multiple of them — so 4 KV
// heads run at degree 2, 4 or 8, and never at 3.
func shardable(f Facts, n int) bool {
	if f.AttentionHeads%n != 0 {
		return false
	}
	if f.KVHeads <= 0 {
		return true
	}
	return f.KVHeads%n == 0 || n%f.KVHeads == 0
}

func largestPowerOfTwo(n int) int {
	p := 1
	for p*2 <= n {
		p *= 2
	}
	return p
}

// ShardVRAM is the VRAM a runtime can allocate across the cards it can reach.
//
// Per-GPU VRAM times the shard count, then the usable fraction — not the
// host's advertised total. A card the engine will not place weights on holds
// no part of the model, and counting it is how a 6-card box gets rented for a
// model that only fits on all six.
func ShardVRAM(vramPerGPUGB, shards int) uint64 {
	if shards < 1 {
		shards = 1
	}
	return UsableVRAM(uint64(vramPerGPUGB) * uint64(shards) * GiB)
}
