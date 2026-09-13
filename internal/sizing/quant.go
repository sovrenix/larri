// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sizing

import (
	"fmt"
	"strings"
)

// BitsPerWeight returns the average storage cost of one parameter under a
// quantization scheme.
//
// The figures are averages including each format's scale and zero-point
// overhead, which is why they are not the round numbers their names suggest:
// q4_K_M stores 4-bit weights but averages nearer 4.85 bits once its
// per-block scales are counted.
//
// Where a figure is uncertain it errs **high**, because the two directions are
// not symmetric. Over-estimating VRAM costs the operator a slightly larger
// card. Under-estimating costs them a card that OOMs on the first request
// after they have already paid to boot it (R-08).
func BitsPerWeight(quant string) (float64, error) {
	q := strings.ToLower(strings.TrimSpace(quant))
	if q == "" {
		return 16, nil // unquantised default
	}
	if b, ok := quantBits[q]; ok {
		return b, nil
	}
	return 0, fmt.Errorf("sizing: unknown quantization %q: expected one of %s",
		quant, strings.Join(CommonQuantizations, ", "))
}

// quantBits maps a quantization name to average bits per weight.
var quantBits = map[string]float64{
	// Unquantised.
	"fp32": 32, "f32": 32, "float32": 32,
	"fp16": 16, "f16": 16, "float16": 16, "half": 16,
	"bf16": 16, "bfloat16": 16,
	"fp8": 8, "f8": 8, "fp8_e4m3": 8, "fp8_e5m2": 8,

	// Post-training quantisation used by vLLM.
	"int8": 8, "w8a8": 8,
	"awq": 4.25, "awq-int4": 4.25, "gptq-int4": 4.25, "gptq": 4.25,
	"int4": 4.25, "w4a16": 4.25,

	// GGUF k-quants, used by llama.cpp. Averages include block scales.
	"q8_0":   8.50,
	"q6_k":   6.56,
	"q5_k_m": 5.67, "q5_k_s": 5.52, "q5_0": 5.50, "q5_1": 6.00,
	"q4_k_m": 4.85, "q4_k_s": 4.58, "q4_0": 4.55, "q4_1": 5.00,
	"q3_k_m": 3.91, "q3_k_s": 3.50, "q3_k_l": 4.27,
	"q2_k":   3.35,
	"iq4_nl": 4.50, "iq4_xs": 4.25,
	"iq3_m": 3.66, "iq3_s": 3.44, "iq3_xs": 3.30, "iq3_xxs": 3.06,
	"iq2_m": 2.70, "iq2_s": 2.50, "iq2_xs": 2.31, "iq2_xxs": 2.06,
	"iq1_m": 1.75, "iq1_s": 1.56,

	// Unsloth's dynamic quantisations, published as UD-* directories and
	// tagged *_XL inside the filenames. They are not the k-quant their name
	// resembles: the method keeps attention and shared-expert tensors at
	// higher precision, so the average lands well above the nominal figure —
	// and furthest above it on the MoE models these are mostly published for,
	// where those tensors are a large share of a small active set.
	//
	// Measured rather than derived, from unsloth/Qwen3.8-Flash-Next-GGUF
	// against the 180B base: UD-IQ1_S is 72.5 GB, which is 3.22 bits per
	// weight and not the 1.56 its name suggests. Sizing those by the nominal
	// figure would have under-committed by a factor of two, and an
	// under-committed plan is one that OOMs after the rig is paid for.
	// Dense models carry less of that uplift, so these err high there — the
	// direction §7.2 requires.
	//
	// The UD prefix has to be typed for the IQ figures to apply, because
	// nothing else distinguishes them: the tag inside a UD file's name is
	// plain "IQ1_M", so a bare --quantization IQ1_M against one of these
	// repositories sizes at the nominal 1.75 and under-commits. The k-quants
	// do not have that problem — their *_XL suffix appears in the filename —
	// and the durable fix for the rest is to size a GGUF from the file sizes
	// the repository publishes rather than from any table.
	"q2_k_xl": 3.51, "ud-q2_k_xl": 3.51,
	"q3_k_xl": 4.00, "ud-q3_k_xl": 4.00,
	"q4_k_xl": 4.95, "ud-q4_k_xl": 4.95,
	"q5_k_xl": 7.03, "ud-q5_k_xl": 7.03,
	"q6_k_xl": 7.52, "ud-q6_k_xl": 7.52,
	"ud-iq1_s": 3.22, "ud-iq1_m": 3.31,
	"ud-iq3_xxs": 3.64, "ud-iq4_xs": 4.16,
}

// CommonQuantizations are the schemes worth naming in an error message. The
// full table is larger; listing all of it would bury the one line the operator
// needs to read.
var CommonQuantizations = []string{
	"fp16", "bf16", "fp8", "awq", "gptq-int4", "q8_0", "q6_K", "q5_K_M", "q4_K_M",
}

// KnownQuantizations lists every recognised scheme, for validation and shell
// completion.
func KnownQuantizations() []string {
	out := make([]string, 0, len(quantBits))
	for k := range quantBits {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// IsGGUF reports whether a quantization names a GGUF k-quant, which selects
// llama.cpp in the runtime heuristic (FR-RT-02).
func IsGGUF(quant string) bool {
	q := strings.ToLower(strings.TrimSpace(quant))
	return strings.HasPrefix(q, "q") || strings.HasPrefix(q, "iq")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
