// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package llamacpp

import (
	"strings"
	"testing"
)

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

// A repository ships more than the model. "mmproj-F16.gguf" is the multimodal
// projector — under a gigabyte beside a fifty-gigabyte model — and because
// selection prefers the shortest matching name, it beat the model outright: a
// live probe resolved --quantization fp16 to mmproj-F16.gguf, which would have
// rented a 128 GB box and handed llama.cpp a file that is not a model.
func TestProjectorsAreNeverSelectedAsTheModel(t *testing.T) {
	files := []string{
		"mmproj-F16.gguf",
		"Qwen3.6-27B-F16.gguf",
		"Qwen3.6-27B-Q4_K_M.gguf",
	}
	got, err := pickQuant("repo", files, "fp16")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Qwen3.6-27B-F16.gguf" {
		t.Errorf("selected %q; the projector is loaded beside the weights, never instead of them", got)
	}
	for _, aux := range []string{"mmproj-F16.gguf", "adapter_model.gguf", "Qwen-lora-Q4_K_M.gguf"} {
		if !auxiliaryGGUF(aux) {
			t.Errorf("%q should be recognised as auxiliary", aux)
		}
	}
	for _, real := range []string{"Qwen3.6-27B-Q4_K_M.gguf", "BF16/Qwen3.6-27B-BF16-00001-of-00002.gguf"} {
		if auxiliaryGGUF(real) {
			t.Errorf("%q is the model and must not be filtered", real)
		}
	}
	// And a repository's advertised quantisations must not include one that
	// exists only as a projector.
	if got := quantsIn(files); len(got) != 2 {
		t.Errorf("quantsIn = %v, want only the two real quantisations", got)
	}
}

// "f16" is a substring of "bf16", so substring matching alone would take a
// BF16 file from a repository carrying both — and which one it got would
// depend on filename length.
func TestFP16DoesNotMatchBF16WhenBothExist(t *testing.T) {
	files := []string{"m-BF16.gguf", "m-F16.gguf"}
	got, err := pickQuant("repo", files, "fp16")
	if err != nil {
		t.Fatal(err)
	}
	if got != "m-F16.gguf" {
		t.Errorf("fp16 selected %q, want the F16 file", got)
	}
	got, err = pickQuant("repo", files, "bf16")
	if err != nil {
		t.Fatal(err)
	}
	if got != "m-BF16.gguf" {
		t.Errorf("bf16 selected %q", got)
	}
}

// Asking a GGUF engine for fp16 requests the one format it exists to avoid.
func TestGGUFEnginesDefaultToAQuantisation(t *testing.T) {
	got := New().DefaultQuantization()
	if got == "" || strings.Contains(strings.ToLower(got), "f16") {
		t.Errorf("DefaultQuantization = %q; llama.cpp exists to run smaller weights", got)
	}
}
