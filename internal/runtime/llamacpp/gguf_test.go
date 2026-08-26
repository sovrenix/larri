// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package llamacpp

import "testing"

// Splitting a GGUF filename on its last dot assumes "model.Q4_K_M.gguf" and
// breaks on the equally common "Qwen3.6-27B-Q4_K_M.gguf", where the dot
// belongs to the model's version. A live run reported the available
// quantisations as "6-27B-Q4_K_M" — strings the operator cannot pass back.
func TestQuantTagSurvivesDotsInTheModelName(t *testing.T) {
	cases := []struct{ file, want string }{
		{"Qwen3.6-27B-Q4_K_M.gguf", "Q4_K_M"},
		{"Qwen3.6-27B-IQ4_XS.gguf", "IQ4_XS"},
		{"Qwen3.6-27B-BF16-00001-of-00002.gguf", "BF16"},
		{"model.Q8_0.gguf", "Q8_0"}, // the other convention still works
		{"repo/dir/Llama-3-8B-F16.gguf", "F16"},
		{"Qwen3.6-27B-Q2_K_XL.gguf", "Q2_K_XL"},
		{"no-quantisation-here.gguf", ""},
	}
	for _, tc := range cases {
		if got := quantTag(tc.file); got != tc.want {
			t.Errorf("quantTag(%q) = %q, want %q", tc.file, got, tc.want)
		}
	}
}

// The float formats have two names each and both are in common use: a
// repository writes "F16" where the operator writes "fp16". A live run
// reported "no fp16 quantisation" about a repository whose listing showed
// F16 on the very next line.
func TestFloatQuantisationsMatchEitherSpelling(t *testing.T) {
	files := []string{"Qwen3.6-27B-F16.gguf", "Qwen3.6-27B-Q4_K_M.gguf"}
	for _, spelling := range []string{"fp16", "f16", "float16", "half"} {
		got, err := pickQuant("r", files, spelling)
		if err != nil {
			t.Errorf("%q should find F16: %v", spelling, err)
			continue
		}
		if got != "Qwen3.6-27B-F16.gguf" {
			t.Errorf("%q selected %q", spelling, got)
		}
	}
	// An exact GGUF name still matches exactly, and a genuine miss still misses.
	if _, err := pickQuant("r", files, "Q5_K_M"); err == nil {
		t.Error("a quantisation the repo lacks must still be an error")
	}
}
