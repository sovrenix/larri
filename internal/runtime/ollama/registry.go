// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/gguf"
)

// Registry is where Ollama tags resolve.
const Registry = "https://registry.ollama.ai"

// headerBytes is how much of the weight blob to read.
//
// The architecture keys sit at the very front of a GGUF header — measured at
// about 1.3 KB on a real model, ahead of the tokenizer vocabulary that makes
// up the rest. A megabyte is generous by three orders of magnitude and still
// nothing next to a multi-gigabyte download.
const headerBytes = 1 << 20

// Info is what the registry and the model's own header say about a tag.
type Info struct {
	Ref           string
	Arch          string
	Layers        int
	KVHeads       int
	HeadDim       int
	HiddenSize    int
	MaxContextLen int

	// Quantization is the model's actual scheme, read from its header rather
	// than taken from whatever the operator typed. An Ollama tag ships one
	// quantisation and the operator does not choose it, so sizing against a
	// guess would be sizing against fiction.
	Quantization  string
	BitsPerWeight float64

	// WeightBytes is the exact size of the weight blob, from the manifest.
	// Better than any estimate, because it is not an estimate.
	WeightBytes int64
}

// Params derives the parameter count in billions from the weight blob and the
// bits the header says each weight occupies.
//
// Back-derived rather than read from general.size_label: the label is a
// marketing round number ("1.5B", "8B") while these two are exact, and the
// number is only ever used to recompute the weight footprint that produced it.
func (i Info) Params() float64 {
	if i.BitsPerWeight <= 0 || i.WeightBytes <= 0 {
		return 0
	}
	return float64(i.WeightBytes) * 8 / i.BitsPerWeight / 1e9
}

type manifest struct {
	Layers []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"layers"`
}

// Inspect resolves a tag to the facts needed to size it, without downloading
// the model.
//
// It runs locally and before anything is rented, so a tag that does not exist
// or cannot be read costs a line of output rather than a rented GPU that
// discovers the problem after the pull.
func Inspect(ctx context.Context, ref string) (Info, error) {
	repo, tag := splitRef(ref)
	cl := &http.Client{Timeout: 45 * time.Second}

	man, err := fetchManifest(ctx, cl, repo, tag)
	if err != nil {
		return Info{}, err
	}
	var digest string
	var size int64
	for _, l := range man.Layers {
		if l.MediaType == "application/vnd.ollama.image.model" {
			digest, size = l.Digest, l.Size
			break
		}
	}
	if digest == "" {
		return Info{}, errs.Newf(errs.ClassModelFailure, "ollama.Inspect",
			"%s has no model layer", ref)
	}

	head, err := fetchBlobPrefix(ctx, cl, repo, digest, headerBytes)
	if err != nil {
		return Info{}, err
	}
	f, err := gguf.Parse(head)
	if err != nil {
		return Info{}, errs.Newf(errs.ClassModelFailure, "ollama.Inspect",
			"read %s header: %v", ref, err)
	}

	info := Info{Ref: ref, Arch: f.Arch(), WeightBytes: size}
	need := func(suffix string) (int, error) {
		v, ok := f.ArchUint(suffix)
		if !ok || v == 0 {
			// A missing architecture key is a refusal, not a default. A
			// fabricated layer count yields a confident VRAM figure that is
			// wrong, and a confident wrong figure gets acted on (§7.1).
			return 0, errs.Newf(errs.ClassModelFailure, "ollama.Inspect",
				"%s header has no %s.%s", ref, f.Arch(), suffix)
		}
		return int(v), nil
	}
	if info.Layers, err = need("block_count"); err != nil {
		return Info{}, err
	}
	if info.HiddenSize, err = need("embedding_length"); err != nil {
		return Info{}, err
	}
	heads, err := need("attention.head_count")
	if err != nil {
		return Info{}, err
	}
	// head_count_kv is absent on models without grouped-query attention,
	// where every head has its own KV — so falling back to head_count is the
	// correct reading rather than a guess.
	if kv, ok := f.ArchUint("attention.head_count_kv"); ok && kv > 0 {
		info.KVHeads = int(kv)
	} else {
		info.KVHeads = heads
	}
	info.HeadDim = info.HiddenSize / heads
	if ctxLen, ok := f.ArchUint("context_length"); ok {
		info.MaxContextLen = int(ctxLen)
	}
	if ft, ok := f.Uint("general.file_type"); ok {
		info.Quantization = gguf.FileTypeName(ft)
		if bits, ok := gguf.FileTypeBits(ft); ok {
			info.BitsPerWeight = bits
		}
	}
	if info.BitsPerWeight == 0 {
		return Info{}, errs.Newf(errs.ClassModelFailure, "ollama.Inspect",
			"%s uses a quantisation larri cannot size (%s)", ref, info.Quantization)
	}
	return info, nil
}

// splitRef turns "qwen2.5:1.5b" into the registry's repo and tag. Bare names
// live under library/, which is what an unqualified tag means.
func splitRef(ref string) (repo, tag string) {
	repo, tag = ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > 0 {
		repo, tag = ref[:i], ref[i+1:]
	}
	if !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	return repo, tag
}

func fetchManifest(ctx context.Context, cl *http.Client, repo, tag string) (*manifest, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", Registry, repo, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	resp, err := cl.Do(req)
	if err != nil {
		return nil, errs.Newf(errs.ClassProviderTransient, "ollama.Inspect",
			"reach the ollama registry: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errs.Newf(errs.ClassModelFailure, "ollama.Inspect",
			"no ollama model %s:%s", repo, tag)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errs.Newf(errs.ClassProviderTransient, "ollama.Inspect",
			"manifest %s:%s: http %d", repo, tag, resp.StatusCode)
	}
	var m manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&m); err != nil {
		return nil, errs.Newf(errs.ClassProviderTransient, "ollama.Inspect",
			"decode manifest: %v", err)
	}
	return &m, nil
}

// fetchBlobPrefix reads the first n bytes of a blob.
//
// A range request, because the blob is the whole model and only its header is
// wanted. A server that ignores the range and sends the lot is handled by the
// limit reader rather than by trust.
func fetchBlobPrefix(ctx context.Context, cl *http.Client, repo, digest string, n int64) ([]byte, error) {
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", Registry, repo, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", n-1))
	resp, err := cl.Do(req)
	if err != nil {
		return nil, errs.Newf(errs.ClassProviderTransient, "ollama.Inspect",
			"read model header: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, errs.Newf(errs.ClassProviderTransient, "ollama.Inspect",
			"model header: http %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, n))
}
