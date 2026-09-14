// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package llamacpp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/secret"
)

// serveListing answers the repository listing for one repo, with sizes, so
// resolution runs end to end without reaching Hugging Face.
func serveListing(t *testing.T, repo string, files map[string]uint64) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/"+repo {
			http.NotFound(w, r)
			return
		}
		var info hfModelInfo
		for name, size := range files {
			info.Siblings = append(info.Siblings, struct {
				RFilename string `json:"rfilename"`
				Size      uint64 `json:"size"`
			}{name, size})
		}
		_ = json.NewEncoder(w).Encode(info)
	}))
	t.Cleanup(srv.Close)
	old := listingEndpoint
	listingEndpoint = srv.URL
	t.Cleanup(func() { listingEndpoint = old })
}

func qwenListing(t *testing.T) {
	serveListing(t, "unsloth/Qwen3-8B-GGUF", map[string]uint64{
		"Qwen3-8B-BF16.gguf":   16_400_000_000,
		"Qwen3-8B-Q4_K_M.gguf": 5_030_000_000,
		"Qwen3-8B-Q8_0.gguf":   8_710_000_000,
	})
}

// The bug: nothing named a quantisation, the default had not been applied
// yet when the repository was listed, and an empty request matched every
// file — full precision won, at three times the download.
func TestAnUnnamedQuantizationResolvesTheDefault(t *testing.T) {
	qwenListing(t)
	r := New()
	w, err := r.ResolveWeights(context.Background(),
		core.ModelSpec{Ref: "unsloth/Qwen3-8B-GGUF"})
	if err != nil {
		t.Fatal(err)
	}
	if w.File != "Qwen3-8B-Q4_K_M.gguf" || w.Quantization != "Q4_K_M" {
		t.Errorf("resolved %s (%s); the default is Q4_K_M", w.File, w.Quantization)
	}
	if r.WeightBytes() != 5_030_000_000 {
		t.Errorf("recorded %d bytes; Bootstrap and sizing need the chosen file's size", r.WeightBytes())
	}
}

func TestANamedQuantizationIsNotOverridden(t *testing.T) {
	qwenListing(t)
	w, err := New().ResolveWeights(context.Background(),
		core.ModelSpec{Ref: "unsloth/Qwen3-8B-GGUF", Quantization: "Q8_0"})
	if err != nil {
		t.Fatal(err)
	}
	if w.File != "Qwen3-8B-Q8_0.gguf" {
		t.Errorf("resolved %s for Q8_0", w.File)
	}
}

// A ref naming a file used to skip the listing, so it was never measured and
// sizing fell back to an estimate. It also broke sizing outright, which asked
// for a repository named after the file.
func TestANamedFileIsMeasuredAndCarriesItsOwnQuantization(t *testing.T) {
	qwenListing(t)
	w, err := New().ResolveWeights(context.Background(),
		core.ModelSpec{Ref: "unsloth/Qwen3-8B-GGUF/Qwen3-8B-Q8_0.gguf"})
	if err != nil {
		t.Fatal(err)
	}
	if w.File != "Qwen3-8B-Q8_0.gguf" || w.Bytes != 8_710_000_000 {
		t.Errorf("resolved %s at %d bytes", w.File, w.Bytes)
	}
	// No default was applied: the file says Q8_0, and a Q4_K_M default
	// would have recorded the rig as something it is not.
	if w.Quantization != "Q8_0" {
		t.Errorf("quantization = %q, want the file's own Q8_0", w.Quantization)
	}
}

// Quantisations live in folders and come in parts. The path inside the
// repository is what downloads, and any part names the whole set.
func TestANamedShardInAFolderResolvesTheWholeSet(t *testing.T) {
	serveListing(t, "unsloth/Llama-3.3-70B-Instruct-GGUF", map[string]uint64{
		"Q6_K/Llama-3.3-70B-Instruct-Q6_K-00001-of-00002.gguf": 30 << 30,
		"Q6_K/Llama-3.3-70B-Instruct-Q6_K-00002-of-00002.gguf": 24 << 30,
	})
	w, err := New().ResolveWeights(context.Background(), core.ModelSpec{
		Ref: "unsloth/Llama-3.3-70B-Instruct-GGUF/Q6_K/Llama-3.3-70B-Instruct-Q6_K-00002-of-00002.gguf"})
	if err != nil {
		t.Fatal(err)
	}
	if w.File != "Q6_K/Llama-3.3-70B-Instruct-Q6_K-00001-of-00002.gguf" {
		t.Errorf("file = %q; llama.cpp is pointed at part one, folder included", w.File)
	}
	if w.Bytes != 54<<30 {
		t.Errorf("bytes = %d; both parts are downloaded", w.Bytes)
	}
}

func TestANamedFileIsCheckedBeforeRenting(t *testing.T) {
	serveListing(t, "unsloth/Qwen3.8-Flash-Next-GGUF", map[string]uint64{
		"Qwen3.8-Flash-Next-Q4_K_M.gguf":                48 << 30,
		"MTP/mtp-Qwen3.8-Flash-Next-shared-Q4_K_M.gguf": 1 << 30,
	})
	for name, c := range map[string]struct{ ref, quant, want string }{
		"a typo": {
			ref: "unsloth/Qwen3.8-Flash-Next-GGUF/Qwen3.8-Flash-Next-Q4_K_S.gguf", want: "has no file",
		},
		"a draft head": {
			ref:  "unsloth/Qwen3.8-Flash-Next-GGUF/MTP/mtp-Qwen3.8-Flash-Next-shared-Q4_K_M.gguf",
			want: "draft head, not the model",
		},
		"a quantisation the file does not carry": {
			ref: "unsloth/Qwen3.8-Flash-Next-GGUF/Qwen3.8-Flash-Next-Q4_K_M.gguf", quant: "Q8_0",
			want: "carries Q4_K_M, not Q8_0",
		},
	} {
		_, err := New().ResolveWeights(context.Background(),
			core.ModelSpec{Ref: c.ref, Quantization: c.quant})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
			continue
		}
		if !errs.Is(err, errs.ClassModelFailure) {
			t.Errorf("%s: %v is not a model failure; another host would not help", name, err)
		}
	}
}

// The operator's own spelling of the quantisation still matches the file.
func TestANamedFileAcceptsAnotherSpellingOfItsQuantization(t *testing.T) {
	serveListing(t, "unsloth/Qwen3.8-Flash-Next-GGUF", map[string]uint64{
		"UD-Q4_K_XL/Qwen3.8-Flash-Next-UD-Q4_K_XL-00001-of-00001.gguf": 50 << 30,
	})
	_, err := ResolveGGUF(context.Background(),
		"unsloth/Qwen3.8-Flash-Next-GGUF/UD-Q4_K_XL/Qwen3.8-Flash-Next-UD-Q4_K_XL-00001-of-00001.gguf",
		"", "ud-q4_k_xl", secret.Secret{})
	if err != nil {
		t.Errorf("the folder's spelling was refused: %v", err)
	}
}

// unsloth publishes Q6_K of Llama-3.3-70B twice, in two folders, 57.9 GB each.
// Adding every file tagged Q6_K priced it at 115.8 GB, and the advice offered
// Q8_0 — 75 GB, larger than what was chosen — as "35% less to fetch".
func TestAQuantisationPublishedTwiceIsOneDownload(t *testing.T) {
	serveListing(t, "unsloth/Llama-3.3-70B-Instruct-GGUF", map[string]uint64{
		"Llama-3.3-70B-Instruct-Q6_K/Llama-3.3-70B-Instruct-Q6_K-00001-of-00002.gguf": 29_900_000_000,
		"Llama-3.3-70B-Instruct-Q6_K/Llama-3.3-70B-Instruct-Q6_K-00002-of-00002.gguf": 28_000_000_000,
		"Q6_K/Llama-3.3-70B-Instruct-Q6_K-00001-of-00002.gguf":                        50_000_000_000,
		"Q6_K/Llama-3.3-70B-Instruct-Q6_K-00002-of-00002.gguf":                        7_900_000_000,
		"Llama-3.3-70B-Instruct-Q8_0/Llama-3.3-70B-Instruct-Q8_0-00001-of-00002.gguf": 39_800_000_000,
		"Llama-3.3-70B-Instruct-Q8_0/Llama-3.3-70B-Instruct-Q8_0-00002-of-00002.gguf": 35_200_000_000,
		"Llama-3.3-70B-Instruct-Q3_K_M.gguf":                                          34_300_000_000,
	})
	info, err := fetchGGUFListing(context.Background(), "unsloth/Llama-3.3-70B-Instruct-GGUF", "", secret.Secret{})
	if err != nil {
		t.Fatal(err)
	}
	// An alternative published twice is sized once, or it reads as twice
	// the download and is never offered.
	if got := ggufSizes(info)["Q6_K"]; got != 57_900_000_000 {
		t.Errorf("Q6_K sized at %d bytes, want one 57.9 GB set", got)
	}
	r := New()
	spec := core.ModelSpec{Ref: "unsloth/Llama-3.3-70B-Instruct-GGUF", Quantization: "Q6_K"}
	if _, err := r.ResolveWeights(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	advice := r.AdviseModel(context.Background(), spec)
	for _, line := range advice {
		if strings.Contains(line, "Q8_0") {
			t.Errorf("offered a larger quantisation as a saving: %s", line)
		}
	}
	if len(advice) != 1 || !strings.Contains(advice[0], "Q3_K_M at 34.3 GB against Q6_K at 57.9 GB") {
		t.Errorf("advice = %q, want Q3_K_M measured against one 57.9 GB set", advice)
	}
}

// Every part is downloaded, and a missing one fails on the rented host after
// the rest are paid for — then again on the next host, since a failed
// download reads as the host's fault. The listing already says so.
func TestAnIncompleteSetIsRefusedBeforeRenting(t *testing.T) {
	serveListing(t, "org/Big-GGUF", map[string]uint64{
		"Q4_K_M/Big-Q4_K_M-00001-of-00003.gguf": 40 << 30,
		"Q4_K_M/Big-Q4_K_M-00003-of-00003.gguf": 12 << 30,
		// part 2 not uploaded
	})
	for name, spec := range map[string]core.ModelSpec{
		"named part two, which is missing":    {Ref: "org/Big-GGUF/Q4_K_M/Big-Q4_K_M-00002-of-00003.gguf"},
		"named part one of an incomplete set": {Ref: "org/Big-GGUF/Q4_K_M/Big-Q4_K_M-00001-of-00003.gguf"},
		"picked from the listing":             {Ref: "org/Big-GGUF", Quantization: "Q4_K_M"},
	} {
		_, err := New().ResolveWeights(context.Background(), spec)
		if err == nil || !strings.Contains(err.Error(), "Big-Q4_K_M-00002-of-00003.gguf") {
			t.Errorf("%s: err = %v, want the missing part named", name, err)
		}
	}
}

// Bootstrap downloads at the spec's revision, so the file is chosen and
// measured from the listing at that revision, not from main.
func TestResolutionReadsTheRevisionItWillDownload(t *testing.T) {
	serveListing(t, "org/Pinned-GGUF/revision/abc123", map[string]uint64{
		"Pinned-Q4_K_M.gguf": 4 << 30,
	})
	w, err := New().ResolveWeights(context.Background(),
		core.ModelSpec{Ref: "org/Pinned-GGUF", Revision: "abc123"})
	if err != nil {
		t.Fatalf("resolved against main, not the pinned revision: %v", err)
	}
	if w.File != "Pinned-Q4_K_M.gguf" {
		t.Errorf("file = %q", w.File)
	}
}

// A file whose name carries no quantisation says nothing about its format.
// Left to the engine default it was recorded, and without published sizes
// sized, as Q4_K_M on no evidence.
func TestAnUntaggedNamedFileNeedsItsQuantisationNamed(t *testing.T) {
	serveListing(t, "org/Plain-GGUF", map[string]uint64{"model.gguf": 5 << 30})
	_, err := New().ResolveWeights(context.Background(), core.ModelSpec{Ref: "org/Plain-GGUF/model.gguf"})
	if err == nil || !strings.Contains(err.Error(), "quantization required") {
		t.Errorf("err = %v; an untagged file must not take the default", err)
	}
	w, err := New().ResolveWeights(context.Background(),
		core.ModelSpec{Ref: "org/Plain-GGUF/model.gguf", Quantization: "Q8_0"})
	if err != nil {
		t.Fatal(err)
	}
	if w.Quantization != "Q8_0" {
		t.Errorf("quantization = %q, want the one named", w.Quantization)
	}
}

// ".GGUF" is a spelling like any other. Part names are derived with the
// extension as listed, or the parts of an uppercase set were neither checked
// nor downloaded.
func TestAnUppercaseSetIsStillASet(t *testing.T) {
	parts := ShardFiles("Q4/Big-Q4_K_M-00001-of-00002.GGUF")
	if len(parts) != 2 || parts[1] != "Q4/Big-Q4_K_M-00002-of-00002.GGUF" {
		t.Fatalf("parts = %v", parts)
	}
	serveListing(t, "org/Big-GGUF", map[string]uint64{
		"Q4/Big-Q4_K_M-00001-of-00002.GGUF": 40 << 30,
	})
	_, err := New().ResolveWeights(context.Background(),
		core.ModelSpec{Ref: "org/Big-GGUF/Q4/Big-Q4_K_M-00001-of-00002.GGUF"})
	if err == nil || !strings.Contains(err.Error(), "Big-Q4_K_M-00002-of-00002.GGUF") {
		t.Errorf("err = %v; the missing second part of an uppercase set must be found", err)
	}
}
