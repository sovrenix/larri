// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sizing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.sovrenix.com/larri/internal/secret"
)

// HFEndpoint is the Hugging Face API host.
const HFEndpoint = "https://huggingface.co"

// HFResolver reads model facts from Hugging Face.
//
// Live rather than bundled (Q-06): config.json is the same file the runtime
// will read, and a catalogue starts drifting the day it ships.
type HFResolver struct {
	Endpoint string
	Token    secret.Secret
	HTTP     *http.Client

	// CacheDir stores resolved facts keyed by commit. Keyed by the resolved
	// SHA rather than a branch, so a hit is a fact about an immutable
	// revision and can never go stale.
	CacheDir string
}

// NewHFResolver builds a resolver with a cache under the user's cache dir.
func NewHFResolver(token secret.Secret) *HFResolver {
	dir, _ := os.UserCacheDir()
	return &HFResolver{
		Endpoint: HFEndpoint,
		Token:    token,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		CacheDir: filepath.Join(dir, "larri", "modelfacts"),
	}
}

// modelInfo is the subset of the model API LARRI reads.
type modelInfo struct {
	SHA         string `json:"sha"`
	Gated       any    `json:"gated"` // false, "auto", or "manual"
	Safetensors *struct {
		Total int64 `json:"total"`
	} `json:"safetensors"`
	Siblings []struct {
		Filename string `json:"rfilename"`
	} `json:"siblings"`
}

// modelConfig is the subset of config.json LARRI reads.
type modelConfig struct {
	NumHiddenLayers   *int `json:"num_hidden_layers"`
	NumAttentionHeads *int `json:"num_attention_heads"`
	NumKeyValueHeads  *int `json:"num_key_value_heads"`
	HiddenSize        *int `json:"hidden_size"`
	HeadDim           *int `json:"head_dim"`
	MaxPositionEmbed  *int `json:"max_position_embeddings"`
	NumExperts        *int `json:"num_local_experts"`
	TextConfig        *struct {
		NumHiddenLayers  *int `json:"num_hidden_layers"`
		NumKeyValueHeads *int `json:"num_key_value_heads"`
		HiddenSize       *int `json:"hidden_size"`
		HeadDim          *int `json:"head_dim"`
		MaxPositionEmbed *int `json:"max_position_embeddings"`
	} `json:"text_config"`
}

// Resolve fetches facts for a model reference.
func (h *HFResolver) Resolve(ctx context.Context, ref, revision string) (Facts, error) {
	if revision == "" {
		revision = "main"
	}
	info, err := h.fetchInfo(ctx, ref, revision)
	if err != nil {
		return Facts{}, err
	}
	// FR-SEC-29: a repository offering only pickle checkpoints executes code
	// when the weights load, on the machine holding the operator's token. The
	// check happens here, during sizing, so the refusal lands before an
	// instance exists rather than after.
	if err := CheckWeightFormat(ref, weightFormat(info)); err != nil {
		return Facts{}, err
	}
	if cached, ok := h.fromCache(ref, info.SHA); ok {
		return cached, nil
	}
	cfg, err := h.fetchConfig(ctx, ref, info.SHA)
	if err != nil {
		return Facts{}, err
	}
	f, err := factsFrom(ref, info, cfg)
	if err != nil {
		return Facts{}, err
	}
	h.toCache(f)
	return f, nil
}

func (h *HFResolver) get(ctx context.Context, path string) ([]byte, error) {
	base := h.Endpoint
	if base == "" {
		base = HFEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	// A gated repository exercises the token here, during sizing. That is a
	// feature: a token that cannot read the repo fails before a second is
	// billed, rather than forty minutes into a bootstrap on a rented A100.
	if !h.Token.Empty() {
		req.Header.Set("Authorization", "Bearer "+h.Token.Reveal())
	}
	cl := h.HTTP
	if cl == nil {
		cl = http.DefaultClient
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sizing: fetch %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return body, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf(
			"sizing: %s is gated or private: set HF_TOKEN to a token that can read it", path)
	case http.StatusNotFound:
		return nil, fmt.Errorf("sizing: %s not found", path)
	default:
		return nil, fmt.Errorf("sizing: fetch %s: http %d", path, resp.StatusCode)
	}
}

func (h *HFResolver) fetchInfo(ctx context.Context, ref, revision string) (*modelInfo, error) {
	b, err := h.get(ctx, "/api/models/"+ref+"/revision/"+revision)
	if err != nil {
		return nil, err
	}
	var info modelInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, fmt.Errorf("sizing: decode model info for %s: %w", ref, err)
	}
	if info.SHA == "" {
		return nil, fmt.Errorf("sizing: %s: no commit reported", ref)
	}
	return &info, nil
}

func (h *HFResolver) fetchConfig(ctx context.Context, ref, sha string) (*modelConfig, error) {
	b, err := h.get(ctx, "/"+ref+"/resolve/"+sha+"/config.json")
	if err != nil {
		return nil, err
	}
	var cfg modelConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("sizing: decode config.json for %s: %w", ref, err)
	}
	return &cfg, nil
}

// weightFormat reports how the repository stores weights.
func weightFormat(info *modelInfo) WeightFormat {
	var sawSafetensors, sawGGUF, sawPickle bool
	for _, s := range info.Siblings {
		n := strings.ToLower(s.Filename)
		switch {
		case strings.HasSuffix(n, ".safetensors"):
			sawSafetensors = true
		case strings.HasSuffix(n, ".gguf"):
			sawGGUF = true
		case strings.HasSuffix(n, ".bin"), strings.HasSuffix(n, ".pt"),
			strings.HasSuffix(n, ".pth"), strings.HasSuffix(n, ".ckpt"):
			sawPickle = true
		}
	}
	switch {
	case sawSafetensors:
		return FormatSafetensors
	case sawGGUF:
		return FormatGGUF
	case sawPickle:
		return FormatPickle
	default:
		return FormatUnknown
	}
}

// factsFrom assembles Facts, refusing rather than guessing when a field the
// estimate depends on is absent.
func factsFrom(ref string, info *modelInfo, cfg *modelConfig) (Facts, error) {
	// Multimodal repositories nest the language model's shape under
	// text_config; using the outer object would size the vision tower.
	layers, kvHeads, hidden, headDim, maxCtx :=
		cfg.NumHiddenLayers, cfg.NumKeyValueHeads, cfg.HiddenSize, cfg.HeadDim, cfg.MaxPositionEmbed
	if t := cfg.TextConfig; t != nil {
		layers = firstInt(t.NumHiddenLayers, layers)
		kvHeads = firstInt(t.NumKeyValueHeads, kvHeads)
		hidden = firstInt(t.HiddenSize, hidden)
		headDim = firstInt(t.HeadDim, headDim)
		maxCtx = firstInt(t.MaxPositionEmbed, maxCtx)
	}
	// Grouped-query attention means kv heads are usually far fewer than
	// attention heads. Falling back to the attention count when the field is
	// absent is correct for older multi-head models, and getting it backwards
	// would inflate the KV estimate eightfold on a modern one.
	if kvHeads == nil {
		kvHeads = cfg.NumAttentionHeads
	}
	f := Facts{Ref: ref, Revision: info.SHA}
	if info.Safetensors != nil && info.Safetensors.Total > 0 {
		f.Params = float64(info.Safetensors.Total) / 1e9
	}
	if layers != nil {
		f.Layers = *layers
	}
	if kvHeads != nil {
		f.KVHeads = *kvHeads
	}
	if hidden != nil {
		f.HiddenSize = *hidden
	}
	switch {
	case headDim != nil:
		f.HeadDim = *headDim
	case hidden != nil && cfg.NumAttentionHeads != nil && *cfg.NumAttentionHeads > 0:
		f.HeadDim = *hidden / *cfg.NumAttentionHeads
	}
	if maxCtx != nil {
		f.MaxContextLen = *maxCtx
	}
	if err := f.Validate(); err != nil {
		return Facts{}, err
	}
	return f, nil
}

func firstInt(a, b *int) *int {
	if a != nil {
		return a
	}
	return b
}

func (h *HFResolver) cachePath(ref, sha string) string {
	if h.CacheDir == "" {
		return ""
	}
	return filepath.Join(h.CacheDir, strings.ReplaceAll(ref, "/", "_")+"@"+sha+".json")
}

func (h *HFResolver) fromCache(ref, sha string) (Facts, bool) {
	p := h.cachePath(ref, sha)
	if p == "" {
		return Facts{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return Facts{}, false
	}
	var f Facts
	if json.Unmarshal(b, &f) != nil || f.Validate() != nil {
		return Facts{}, false
	}
	return f, true
}

func (h *HFResolver) toCache(f Facts) {
	p := h.cachePath(f.Ref, f.Revision)
	if p == "" {
		return
	}
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	if b, err := json.Marshal(f); err == nil {
		_ = os.WriteFile(p, b, 0o600)
	}
}
