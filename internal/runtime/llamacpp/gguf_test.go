// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package llamacpp

import (
	"context"
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/secret"
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

// llama.cpp finds the remaining shards for itself once it holds the first,
// but it cannot find what was never fetched. Only shard one was downloaded,
// and the engine then failed on a missing tensor in a way that reads exactly
// like a corrupt transfer.
func TestShardFilesListsEveryPart(t *testing.T) {
	got := ShardFiles("UD-Q4_K_XL/Qwen3.8-Flash-Next-UD-Q4_K_XL-00001-of-00004.gguf")
	want := []string{
		"UD-Q4_K_XL/Qwen3.8-Flash-Next-UD-Q4_K_XL-00001-of-00004.gguf",
		"UD-Q4_K_XL/Qwen3.8-Flash-Next-UD-Q4_K_XL-00002-of-00004.gguf",
		"UD-Q4_K_XL/Qwen3.8-Flash-Next-UD-Q4_K_XL-00003-of-00004.gguf",
		"UD-Q4_K_XL/Qwen3.8-Flash-Next-UD-Q4_K_XL-00004-of-00004.gguf",
	}
	if len(got) != len(want) {
		t.Fatalf("listed %d parts, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("part %d = %q, want %q", i+1, got[i], want[i])
		}
	}
}

// A single-file model has to work through the same path, or the caller grows
// a special case that will eventually disagree with this one.
func TestShardFilesReturnsASingleFileUnchanged(t *testing.T) {
	got := ShardFiles("model-Q4_K_M.gguf")
	if len(got) != 1 || got[0] != "model-Q4_K_M.gguf" {
		t.Errorf("ShardFiles on an unsplit model = %v", got)
	}
}

// GGUF repositories publish quantisations in directories. The download created
// the model directory but never the one inside it, so curl failed to open its
// destination before a byte moved.
func TestLocalNameFlattensARepositoryPath(t *testing.T) {
	if got := localName("UD-Q4_K_XL/model-00001-of-00004.gguf"); got != "model-00001-of-00004.gguf" {
		t.Errorf("localName = %q, want the base name", got)
	}
	if got := localName("model.gguf"); got != "model.gguf" {
		t.Errorf("localName on a bare name = %q", got)
	}
}

// The multi-token-prediction head carries the quantisation tag in its own
// name, so it matched exactly the string the operator asked for — and being
// the shortest name, it then won. A live dry run resolved --quantization
// Q4_K_M to a one-gigabyte draft head and planned to rent a 192 GB box for it.
func TestMTPDraftHeadIsNotTheModel(t *testing.T) {
	files := []string{
		"MTP/mtp-Qwen3.8-Flash-Next-Q4_K_M.gguf",
		"MTP/mtp-Qwen3.8-Flash-Next-shared-Q4_K_M.gguf",
		"Q4_K_M/Qwen3.8-Flash-Next-Q4_K_M-00001-of-00004.gguf",
		"Q4_K_M/Qwen3.8-Flash-Next-Q4_K_M-00002-of-00004.gguf",
	}
	got, err := pickQuant("unsloth/x-GGUF", files, "Q4_K_M")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Q4_K_M/Qwen3.8-Flash-Next-Q4_K_M-00001-of-00004.gguf" {
		t.Errorf("resolved %q; the draft head is not the model", got)
	}
}

// A repository that carries no such quantisation must say what it does carry,
// rather than reach for whatever else matched the string.
func TestAMissingQuantisationNamesWhatTheRepoHas(t *testing.T) {
	files := []string{
		"MTP/mtp-x-Q4_K_M.gguf",
		"UD-Q4_K_XL/x-UD-Q4_K_XL-00001-of-00004.gguf",
		"Q8_0/x-Q8_0-00001-of-00006.gguf",
	}
	_, err := pickQuant("unsloth/x-GGUF", files, "Q4_K_M")
	if err == nil {
		t.Fatal("a repository with no Q4_K_M resolved one anyway")
	}
	for _, want := range []string{"Q4_K_XL", "Q8_0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s, which the repo has: %v", want, err)
		}
	}
}

// "UD-Q4_K_XL" is the name on the directory the operator was reading, and it
// matched nothing: the tag inside the filename is "Q4_K_XL".
func TestUnslothDynamicQuantSpellingResolves(t *testing.T) {
	files := []string{"UD-Q4_K_XL/x-UD-Q4_K_XL-00001-of-00004.gguf"}
	got, err := pickQuant("unsloth/x-GGUF", files, "UD-Q4_K_XL")
	if err != nil {
		t.Fatalf("the repository's own directory name did not resolve: %v", err)
	}
	if got != files[0] {
		t.Errorf("resolved %q", got)
	}
}

// The size that decides how much VRAM to rent should be measured, not
// estimated. The listing already carries it, so summing the shards costs
// nothing and beats a parameter count times a table of averages.
func TestShardBytesSumsTheWholeQuantisation(t *testing.T) {
	sizes := map[string]uint64{
		"UD-Q4_K_XL/m-00001-of-00003.gguf": 40 << 30,
		"UD-Q4_K_XL/m-00002-of-00003.gguf": 40 << 30,
		"UD-Q4_K_XL/m-00003-of-00003.gguf": 31 << 30,
		"Q8_0/m-00001-of-00002.gguf":       99 << 30,
	}
	got := shardBytes("UD-Q4_K_XL/m-00001-of-00003.gguf", sizes)
	if want := uint64(111) << 30; got != want {
		t.Errorf("summed %d, want %d — a quantisation is all of its shards", got, want)
	}
}

// All of them or none. A partial total would be believed, and believing a
// figure that is missing a 40 GB shard is how a rig is sized for two thirds
// of a model.
func TestShardBytesRefusesAPartialTotal(t *testing.T) {
	sizes := map[string]uint64{
		"m-00001-of-00003.gguf": 40 << 30,
		"m-00003-of-00003.gguf": 31 << 30,
		// shard 2 absent
	}
	if got := shardBytes("m-00001-of-00003.gguf", sizes); got != 0 {
		t.Errorf("reported %d from an incomplete listing; 0 means 'estimate instead'", got)
	}
	// A zero-sized entry is the same absence wearing a number.
	sizes["m-00002-of-00003.gguf"] = 0
	if got := shardBytes("m-00001-of-00003.gguf", sizes); got != 0 {
		t.Errorf("reported %d from a listing with a zero size", got)
	}
}

// A ref naming a file outright has no listing to take a size from, and must
// still resolve — sizing falls back to the estimate, as it did for every model
// before sizes were read at all.
func TestExplicitRefResolvesWithoutASize(t *testing.T) {
	w, err := ResolveGGUF(context.Background(),
		"org/repo/model-Q4_K_M.gguf", "Q4_K_M", secret.Secret{})
	if err != nil {
		t.Fatal(err)
	}
	if w.File != "model-Q4_K_M.gguf" {
		t.Errorf("file = %q", w.File)
	}
	if w.Bytes != 0 {
		t.Errorf("bytes = %d; nothing measured it, so it must say so", w.Bytes)
	}
}
