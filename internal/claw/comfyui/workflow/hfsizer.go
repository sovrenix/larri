// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.sovrenix.com/larri/internal/buildinfo"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/secret"
)

// HFEndpoint is the default weight host.
const HFEndpoint = "https://huggingface.co"

// HFSizer measures files on Hugging Face without downloading them.
//
// One listing per repository, cached, rather than one request per file: a
// FLUX graph names the transformer, the autoencoder and two text encoders
// across three repositories, and asking separately for each file would be
// four round-trips where two suffice. The listing carries LFS sizes, which is
// the number that matters — a naive HEAD on the file path returns the size of
// the pointer, about 130 bytes, and a cold-start estimate built on that would
// rank every host as instant.
type HFSizer struct {
	Token    secret.Secret
	Endpoint string
	HTTP     *http.Client

	mu    sync.Mutex
	repos map[string]map[string]uint64 // repo@revision -> file -> bytes
}

// NewHFSizer builds a sizer.
func NewHFSizer(token secret.Secret) *HFSizer {
	return &HFSizer{Token: token, repos: map[string]map[string]uint64{}}
}

var _ Sizer = (*HFSizer)(nil)

// Size reports the byte length of one file in a repository.
func (h *HFSizer) Size(ctx context.Context, repo, revision, file string) (uint64, error) {
	files, err := h.listing(ctx, repo, revision)
	if err != nil {
		return 0, err
	}
	// A zero is not a measurement. Hugging Face reports no size for a file
	// whose LFS pointer it has not resolved, and taking that at face value
	// planned the model at nothing: VRAM and disk understated, and the fetch
	// script's own size check skipped, since it only verifies a positive
	// expectation. §4a says measure the weights rather than estimate them,
	// and zero is worse than an estimate — it is an estimate that always
	// fits.
	if n, ok := files[file]; ok {
		if n == 0 {
			return 0, errs.Newf(errs.ClassModelFailure, "workflow.Size",
				"%s in %s lists no size", file, repo)
		}
		return n, nil
	}
	// Graphs address models through ComfyUI's directory layout, so a name may
	// carry a folder the repository does not use.
	if n, ok := files[baseName(file)]; ok {
		if n == 0 {
			return 0, errs.Newf(errs.ClassModelFailure, "workflow.Size",
				"%s in %s lists no size", baseName(file), repo)
		}
		return n, nil
	}
	return 0, errs.Newf(errs.ClassModelFailure, "workflow.Size",
		"no file %s in %s", file, repo)
}

// listing fetches and caches one repository's file sizes.
func (h *HFSizer) listing(ctx context.Context, repo, revision string) (map[string]uint64, error) {
	key := repo + "@" + revision
	h.mu.Lock()
	cached, ok := h.repos[key]
	h.mu.Unlock()
	if ok {
		return cached, nil
	}

	base := h.Endpoint
	if base == "" {
		base = HFEndpoint
	}
	url := base + "/api/models/" + repo
	if revision != "" {
		url += "/revision/" + revision
	}
	url += "?blobs=true"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	// A gated repository exercises the token here, during sizing, which is
	// the whole value of doing this before the create call: a token that
	// cannot read FLUX.1-dev fails now rather than on a rented H100.
	if !h.Token.Empty() {
		req.Header.Set("Authorization", "Bearer "+h.Token.Reveal())
	}
	cl := h.HTTP
	if cl == nil {
		cl = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, errs.Newf(errs.ClassProviderTransient, "workflow.Size",
			"list %s: %v", repo, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, errs.Newf(errs.ClassModelFailure, "workflow.Size",
			"%s is gated or private: set HF_TOKEN to a token that can read it", repo)
	case http.StatusNotFound:
		return nil, errs.Newf(errs.ClassModelFailure, "workflow.Size",
			"no repository %s", repo)
	default:
		return nil, errs.Newf(errs.ClassProviderTransient, "workflow.Size",
			"list %s: http %d", repo, resp.StatusCode)
	}

	var doc struct {
		Siblings []struct {
			Name string `json:"rfilename"`
			Size uint64 `json:"size"`
		} `json:"siblings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&doc); err != nil {
		return nil, errs.Newf(errs.ClassProviderTransient, "workflow.Size",
			"decode %s: %v", repo, err)
	}
	files := make(map[string]uint64, len(doc.Siblings))
	for _, s := range doc.Siblings {
		files[s.Name] = s.Size
		// Also keyed by base name, so a graph naming "vae/sdxl_vae.safetensors"
		// finds a repository that publishes it at the root.
		if b := baseName(s.Name); b != s.Name {
			if _, clash := files[b]; !clash {
				files[b] = s.Size
			}
		}
	}
	h.mu.Lock()
	h.repos[key] = files
	h.mu.Unlock()
	return files, nil
}

// DownloadURL is where an item's bytes are fetched from on the host.
//
// Built here rather than on the host so the endpoint override — a mirror, for
// a region that cannot route to huggingface.co — applies to ComfyUI's assets
// exactly as it does to an LLM's weights.
func (h *HFSizer) DownloadURL(src Source) string {
	base := h.Endpoint
	if base == "" {
		base = HFEndpoint
	}
	rev := src.Revision
	if rev == "" {
		rev = "main"
	}
	return fmt.Sprintf("%s/%s/resolve/%s/%s",
		strings.TrimRight(base, "/"), src.Repo, rev, src.File)
}

// StaticSizer answers from a table. Tests and manifests that already carry
// sizes use it, so resolution is exercisable with no network at all.
type StaticSizer map[string]uint64

var _ Sizer = StaticSizer(nil)

// Size reports a recorded size, keyed by "repo/file".
func (s StaticSizer) Size(_ context.Context, repo, _, file string) (uint64, error) {
	if n, ok := s[repo+"/"+file]; ok {
		return n, nil
	}
	if n, ok := s[file]; ok {
		return n, nil
	}
	return 0, errs.Newf(errs.ClassModelFailure, "workflow.Size",
		"no file %s in %s", file, repo)
}
