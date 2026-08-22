// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sizing

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/secret"
)

// A realistic Llama-3.1-8B pair: model info carries the parameter count and
// file list, config.json carries the architecture.
const (
	infoJSON = `{"sha":"abc123def456","gated":false,
	  "safetensors":{"total":8030261248},
	  "siblings":[{"rfilename":"config.json"},
	              {"rfilename":"model-00001-of-00004.safetensors"},
	              {"rfilename":"tokenizer.json"}]}`
	configJSON = `{"num_hidden_layers":32,"num_attention_heads":32,
	  "num_key_value_heads":8,"hidden_size":4096,"head_dim":128,
	  "max_position_embeddings":131072,"vocab_size":128256,
	  "rope_theta":500000.0,"torch_dtype":"bfloat16"}`
)

func hfServer(t *testing.T, info, config string, hdr *string) *HFResolver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hdr != nil {
			*hdr = r.Header.Get("Authorization")
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/models/"):
			fmt.Fprint(w, info)
		case strings.Contains(r.URL.Path, "config.json"):
			fmt.Fprint(w, config)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &HFResolver{Endpoint: srv.URL, HTTP: srv.Client(), CacheDir: t.TempDir()}
}

func TestResolveRealisticModel(t *testing.T) {
	r := hfServer(t, infoJSON, configJSON, nil)
	f, err := r.Resolve(context.Background(), "meta-llama/Llama-3.1-8B", "main")
	if err != nil {
		t.Fatal(err)
	}
	if f.Layers != 32 || f.HiddenSize != 4096 || f.HeadDim != 128 {
		t.Errorf("architecture = %+v", f)
	}
	// GQA: 8 kv heads, not the 32 attention heads. Reading the wrong field
	// inflates the KV estimate eightfold and makes every 70B unservable.
	if f.KVHeads != 8 {
		t.Errorf("KVHeads = %d, want 8 (num_key_value_heads, not attention heads)", f.KVHeads)
	}
	if f.Params < 8.0 || f.Params > 8.1 {
		t.Errorf("Params = %.3f B, want ~8.03", f.Params)
	}
	// Keyed by resolved commit, so a cache hit is a fact about an immutable
	// revision rather than about whatever main pointed at that day.
	if f.Revision != "abc123def456" {
		t.Errorf("Revision = %q, want the resolved sha", f.Revision)
	}
}

// FR-SEC-29: pickle checkpoints deserialise into live objects, which is code
// execution on the host holding the operator's token. Refused during sizing,
// before an instance exists.
func TestPickleOnlyRepositoryIsRefusedBeforeAnyInstance(t *testing.T) {
	pickleInfo := `{"sha":"abc","safetensors":null,
	  "siblings":[{"rfilename":"pytorch_model.bin"},{"rfilename":"config.json"}]}`
	r := hfServer(t, pickleInfo, configJSON, nil)
	_, err := r.Resolve(context.Background(), "sketchy/model", "main")
	if err == nil {
		t.Fatal("a pickle-only repository must be refused")
	}
	if !strings.Contains(err.Error(), "safetensors required") {
		t.Errorf("the refusal should name the requirement: %v", err)
	}
}

func TestSafetensorsAlongsidePickleIsAccepted(t *testing.T) {
	mixed := `{"sha":"abc","safetensors":{"total":8030261248},
	  "siblings":[{"rfilename":"pytorch_model.bin"},
	              {"rfilename":"model.safetensors"}]}`
	r := hfServer(t, mixed, configJSON, nil)
	if _, err := r.Resolve(context.Background(), "mixed/model", "main"); err != nil {
		t.Fatalf("a repo offering safetensors is fine even if a pickle sits beside it: %v", err)
	}
}

// A gated repo exercises the token during sizing, so a bad one fails before a
// second is billed rather than forty minutes into a bootstrap.
func TestGatedRepoFailsDuringSizingNotBootstrap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	r := &HFResolver{Endpoint: srv.URL, HTTP: srv.Client(), CacheDir: t.TempDir()}
	_, err := r.Resolve(context.Background(), "meta-llama/Llama-3.1-70B", "main")
	if err == nil {
		t.Fatal("a gated repo with no usable token must fail")
	}
	if !strings.Contains(err.Error(), "HF_TOKEN") {
		t.Errorf("the error should name the credential to set: %v", err)
	}
}

func TestTokenIsSentWhenPresent(t *testing.T) {
	var seen string
	r := hfServer(t, infoJSON, configJSON, &seen)
	r.Token = secret.New("hf_secrettoken")
	if _, err := r.Resolve(context.Background(), "x/y", "main"); err != nil {
		t.Fatal(err)
	}
	if seen != "Bearer hf_secrettoken" {
		t.Errorf("auth header = %q", seen)
	}
}

// Multimodal repositories nest the language model's shape under text_config.
// Sizing the outer object would measure the vision tower instead.
func TestTextConfigWinsForMultimodalRepos(t *testing.T) {
	mm := `{"num_hidden_layers":2,"hidden_size":1024,"num_attention_heads":16,
	  "text_config":{"num_hidden_layers":80,"num_key_value_heads":8,
	                 "hidden_size":8192,"head_dim":128,
	                 "max_position_embeddings":131072}}`
	r := hfServer(t, infoJSON, mm, nil)
	f, err := r.Resolve(context.Background(), "meta-llama/Llama-3.2-90B-Vision", "main")
	if err != nil {
		t.Fatal(err)
	}
	if f.Layers != 80 || f.HiddenSize != 8192 {
		t.Errorf("used the outer config instead of text_config: %+v", f)
	}
}

// Older multi-head models omit num_key_value_heads; falling back to the
// attention count is correct for them.
func TestMissingKVHeadsFallsBackToAttentionHeads(t *testing.T) {
	old := `{"num_hidden_layers":32,"num_attention_heads":32,"hidden_size":4096,
	  "max_position_embeddings":4096}`
	r := hfServer(t, infoJSON, old, nil)
	f, err := r.Resolve(context.Background(), "old/model", "main")
	if err != nil {
		t.Fatal(err)
	}
	if f.KVHeads != 32 {
		t.Errorf("KVHeads = %d, want the attention head count for a pre-GQA model", f.KVHeads)
	}
	if f.HeadDim != 128 {
		t.Errorf("HeadDim = %d, want hidden/heads = 128", f.HeadDim)
	}
}

func TestIncompleteConfigIsRefusedNotGuessed(t *testing.T) {
	sparse := `{"hidden_size":4096}`
	r := hfServer(t, infoJSON, sparse, nil)
	if _, err := r.Resolve(context.Background(), "x/y", "main"); err == nil {
		t.Fatal("a config missing the layer count must be an error, not an estimate")
	}
}

func TestCacheIsKeyedByCommit(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			fmt.Fprint(w, infoJSON)
			return
		}
		fmt.Fprint(w, configJSON)
	}))
	defer srv.Close()
	r := &HFResolver{Endpoint: srv.URL, HTTP: srv.Client(), CacheDir: t.TempDir()}

	if _, err := r.Resolve(context.Background(), "x/y", "main"); err != nil {
		t.Fatal(err)
	}
	first := calls
	if _, err := r.Resolve(context.Background(), "x/y", "main"); err != nil {
		t.Fatal(err)
	}
	// The info call still happens — it resolves the commit — but config.json
	// is served from cache.
	if calls >= first*2 {
		t.Errorf("cache did not reduce fetches: %d then %d", first, calls)
	}
}
