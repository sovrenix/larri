// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sizing

import "testing"

func TestDetectQuant(t *testing.T) {
	cases := []struct {
		ref  string
		tags []string
		want string
	}{
		{"Intel/Qwen3.6-27B-int4-AutoRound", nil, "int4"},
		{"cyankiwi/Qwen3.6-27B-AWQ-INT4", nil, "awq"},
		{"bottlecapai/ThinkingCap-Qwen3.6-27B-FP8", nil, "fp8"},
		{"unsloth/Qwen3.6-27B-GGUF", nil, "gguf"},
		{"mlx-community/Qwen3.6-27B-4bit", nil, "mlx"},
		{"Qwen/Qwen3.6-27B", nil, ""},
		// Tags carry it when the name does not.
		{"someone/Qwen3.6-27B-small", []string{"gptq", "text-generation"}, "gptq"},
	}
	for _, tc := range cases {
		if got := DetectQuant(tc.ref, tc.tags); got != tc.want {
			t.Errorf("DetectQuant(%q, %v) = %q, want %q", tc.ref, tc.tags, got, tc.want)
		}
	}
}

// A search for "Qwen3.6-27B" also returns fine-tunes and unrelated projects
// that merely share a prefix. Suggesting one of those as "the same model,
// smaller" would be wrong in a way an operator could not easily catch.
func TestOnlySiblingsOfTheSameModelQualify(t *testing.T) {
	base := BaseName("Qwen/Qwen3.6-27B")
	if base != "Qwen3.6-27B" {
		t.Fatalf("BaseName = %q", base)
	}
	for _, ok := range []string{
		"Intel/Qwen3.6-27B-int4-AutoRound",
		"unsloth/Qwen3.6-27B-GGUF",
	} {
		if !looksLikeSameModel(ok, base) {
			t.Errorf("%q should match %q", ok, base)
		}
	}
	for _, no := range []string{
		"Qwen/Qwen3.6-8B",     // a different size
		"Qwen/Qwen3.5-27B",    // a different generation
		"someone/Llama-3-70B", // unrelated
	} {
		if looksLikeSameModel(no, base) {
			t.Errorf("%q must not be taken for %q", no, base)
		}
	}
}

// Pickle checkpoints execute arbitrary code on load, on the machine holding
// the operator's Hugging Face token. A variant that ships only .bin is not a
// saving worth having.
func TestVariantMustShipSafetensors(t *testing.T) {
	pickleOnly := hfBlobs{Siblings: []struct {
		Name string `json:"rfilename"`
		Size uint64 `json:"size"`
	}{{"pytorch_model.bin", 1 << 30}}}
	if _, safe := weightBytes(pickleOnly); safe {
		t.Error("a .bin-only repo must not report safetensors")
	}
	mixed := hfBlobs{Siblings: []struct {
		Name string `json:"rfilename"`
		Size uint64 `json:"size"`
	}{{"model-00001.safetensors", 2 << 30}, {"model-00002.safetensors", 3 << 30}, {"README.md", 4096}}}
	total, safe := weightBytes(mixed)
	if !safe {
		t.Error("safetensors should be detected")
	}
	if total != 5<<30 {
		t.Errorf("total = %d, want %d (README must not count)", total, 5<<30)
	}
}

func TestSavingOver(t *testing.T) {
	v := Variant{WeightBytes: 19 << 30}
	if got := v.SavingOver(56 << 30); got < 0.65 || got > 0.67 {
		t.Errorf("SavingOver = %.3f, want ~0.66", got)
	}
	// A variant that is not smaller offers nothing.
	if got := (Variant{WeightBytes: 60 << 30}).SavingOver(56 << 30); got != 0 {
		t.Errorf("a larger variant should report no saving, got %v", got)
	}
	if got := v.SavingOver(0); got != 0 {
		t.Errorf("unknown base size should report no saving, got %v", got)
	}
}
