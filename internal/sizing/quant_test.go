// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sizing

import "testing"

func TestBitsPerWeight(t *testing.T) {
	cases := map[string]float64{
		"":       16, // unquantised default
		"fp16":   16,
		"BF16":   16, // case-insensitive
		" fp32 ": 32, // whitespace-tolerant
		"fp8":    8,
		"q8_0":   8.50,
		"q4_K_M": 4.85,
		"awq":    4.25,
	}
	for in, want := range cases {
		got, err := BitsPerWeight(in)
		if err != nil {
			t.Errorf("BitsPerWeight(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("BitsPerWeight(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := BitsPerWeight("q4_k_ultra"); err == nil {
		t.Error("an unknown scheme must be refused rather than defaulted")
	}
}

// The k-quant figures include block scales, so they sit above the bit count in
// their names. Erring high is deliberate: over-estimating costs a bigger card,
// under-estimating costs a card that OOMs after the operator has paid to boot
// it (R-08).
func TestKQuantFiguresIncludeBlockOverhead(t *testing.T) {
	q4, _ := BitsPerWeight("q4_K_M")
	if q4 <= 4.0 {
		t.Errorf("q4_K_M = %v bits; the name says 4 but block scales push it higher, "+
			"and rounding down here is the direction that OOMs", q4)
	}
	q5, _ := BitsPerWeight("q5_K_M")
	q6, _ := BitsPerWeight("q6_K")
	if !(q4 < q5 && q5 < q6) {
		t.Errorf("quantisation levels must order monotonically: q4=%v q5=%v q6=%v", q4, q5, q6)
	}
}

func TestIsGGUFSelectsLlamaCpp(t *testing.T) {
	for _, q := range []string{"q4_K_M", "q8_0", "iq3_xxs"} {
		if !IsGGUF(q) {
			t.Errorf("%s should be recognised as a GGUF k-quant", q)
		}
	}
	for _, q := range []string{"fp16", "awq", "gptq-int4", ""} {
		if IsGGUF(q) {
			t.Errorf("%s is not a GGUF k-quant", q)
		}
	}
}

func TestKnownQuantizationsIsSortedAndNonEmpty(t *testing.T) {
	ks := KnownQuantizations()
	if len(ks) < 10 {
		t.Fatalf("expected a populated table, got %d", len(ks))
	}
	for i := 1; i < len(ks); i++ {
		if ks[i-1] > ks[i] {
			t.Fatalf("not sorted at %d: %q > %q", i, ks[i-1], ks[i])
		}
	}
}
