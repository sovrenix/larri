// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package llamacpp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/secret"
)

// GGUFFile reports which file in a repository to download.
//
// A repository is not a model here: a GGUF repo holds one file per quantisation
// — a dozen or more — and downloading the wrong one costs the whole transfer at
// rented-GPU prices before anything reveals the mistake. So this resolves
// rather than guesses.
//
// Two forms are accepted. A ref naming a file directly ("repo/owner/x.gguf")
// is taken at its word. Otherwise the repository is listed and the file is
// matched against the requested quantisation, which is the only reliable way:
// naming conventions across GGUF publishers agree on almost nothing except that
// the quantisation appears somewhere in the name.
func GGUFFile(spec core.ModelSpec) (string, error) {
	if f := explicitFile(spec.Ref); f != "" {
		return f, nil
	}
	return "", errs.Newf(errs.ClassModelFailure, "llamacpp.GGUFFile",
		"gguf file for %s not resolved: call ResolveGGUF before bootstrap", spec.Ref)
}

// explicitFile returns the filename when a ref names one outright.
func explicitFile(ref string) string {
	if !strings.HasSuffix(strings.ToLower(ref), ".gguf") {
		return ""
	}
	i := strings.LastIndex(ref, "/")
	if i < 0 {
		return ref
	}
	return ref[i+1:]
}

// RepoOf strips a trailing filename from a ref, leaving the repository.
func RepoOf(ref string) string {
	if explicitFile(ref) == "" {
		return ref
	}
	parts := strings.Split(ref, "/")
	if len(parts) <= 2 {
		return ref
	}
	return strings.Join(parts[:2], "/")
}

type hfModelInfo struct {
	Siblings []struct {
		RFilename string `json:"rfilename"`
	} `json:"siblings"`
}

// ResolveGGUF picks the file matching the requested quantisation.
//
// It runs locally, before anything is rented, so a repository that does not
// carry the requested quantisation is a line of output rather than a bill. The
// error lists what the repo *does* carry, because "not found" without the
// alternatives leaves the operator to go and look it up themselves.
func ResolveGGUF(ctx context.Context, ref, quant string, token secret.Secret) (string, error) {
	if f := explicitFile(ref); f != "" {
		return f, nil
	}
	repo := RepoOf(ref)
	url := "https://huggingface.co/api/models/" + repo

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if !token.Empty() {
		req.Header.Set("Authorization", "Bearer "+token.Reveal())
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", errs.Newf(errs.ClassProviderTransient, "llamacpp.ResolveGGUF",
			"list %s: %v", repo, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", errs.Newf(errs.ClassModelFailure, "llamacpp.ResolveGGUF",
			"no repository %s", repo)
	}
	if resp.StatusCode != http.StatusOK {
		return "", errs.Newf(errs.ClassProviderTransient, "llamacpp.ResolveGGUF",
			"list %s: http %d", repo, resp.StatusCode)
	}
	var info hfModelInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&info); err != nil {
		return "", errs.Newf(errs.ClassProviderTransient, "llamacpp.ResolveGGUF",
			"decode %s: %v", repo, err)
	}

	var ggufs []string
	for _, s := range info.Siblings {
		if strings.HasSuffix(strings.ToLower(s.RFilename), ".gguf") {
			ggufs = append(ggufs, s.RFilename)
		}
	}
	if len(ggufs) == 0 {
		return "", errs.Newf(errs.ClassModelFailure, "llamacpp.ResolveGGUF",
			"%s holds no gguf files", repo)
	}
	return pickQuant(repo, ggufs, quant)
}

// pickQuant chooses among a repository's GGUF files.
func pickQuant(repo string, files []string, quant string) (string, error) {
	sort.Strings(files)

	// A multi-part GGUF is named "…-00001-of-00003.gguf" and llama.cpp loads
	// the remaining shards itself, so only the first is ever the answer.
	// Offering shard 2 would produce a load failure that looks like a corrupt
	// download.
	isLaterShard := func(f string) bool {
		l := strings.ToLower(f)
		i := strings.Index(l, "-of-")
		if i < 0 {
			return false
		}
		return !strings.Contains(l, "-00001-of-")
	}

	q := strings.ToLower(strings.TrimSpace(quant))
	var candidates []string
	for _, f := range files {
		if isLaterShard(f) {
			continue
		}
		if q == "" || strings.Contains(strings.ToLower(f), q) {
			candidates = append(candidates, f)
		}
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	if len(candidates) > 1 {
		// Prefer the shortest name: among "…q4_k_m.gguf" and
		// "…q4_k_m-imat.gguf" the plain one is what was asked for.
		sort.Slice(candidates, func(i, j int) bool {
			return len(candidates[i]) < len(candidates[j])
		})
		return candidates[0], nil
	}
	return "", errs.Newf(errs.ClassModelFailure, "llamacpp.ResolveGGUF",
		"%s has no %s quantisation; it carries: %s",
		repo, quant, strings.Join(quantsIn(files), ", "))
}

// quantsIn summarises what a repository offers, so a miss is actionable.
func quantsIn(files []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		base := strings.TrimSuffix(f[strings.LastIndex(f, "/")+1:], ".gguf")
		parts := strings.Split(base, ".")
		tag := parts[len(parts)-1]
		if i := strings.LastIndex(tag, "-"); i >= 0 && len(parts) == 1 {
			tag = tag[i+1:]
		}
		if tag != "" && !seen[tag] {
			seen[tag] = true
			out = append(out, tag)
		}
	}
	sort.Strings(out)
	if len(out) > 12 {
		out = append(out[:12], fmt.Sprintf("… and %d more", len(out)-12))
	}
	return out
}
