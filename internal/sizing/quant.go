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
	"iq4_xs": 4.25, "iq3_xxs": 3.06, "iq2_xxs": 2.06,
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
