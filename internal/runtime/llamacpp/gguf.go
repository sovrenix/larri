// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package llamacpp

import (
	"context"
	"encoding/json"
	"fmt"
	"go.sovrenix.com/larri/internal/sizing"
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
		Size      uint64 `json:"size"`
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
	info, err := fetchGGUFListing(ctx, repo, token)
	if err != nil {
		return "", err
	}

	var ggufs []string
	for _, s := range info.Siblings {
		if strings.HasSuffix(strings.ToLower(s.RFilename), ".gguf") {
			ggufs = append(ggufs, s.RFilename)
		}
	}
	if len(ggufs) == 0 {
		// The commonest way to reach this: an operator names the original
		// weights, which are safetensors, because that is the repository the
		// model is known by. A GGUF conversion almost always exists under a
		// different account, and naming it turns a dead end into one edit.
		if alt := suggestGGUFRepo(ctx, repo, token); alt != "" {
			return "", errs.Newf(errs.ClassModelFailure, "llamacpp.ResolveGGUF",
				"%s holds no gguf files: try %s", repo, alt)
		}
		return "", errs.Newf(errs.ClassModelFailure, "llamacpp.ResolveGGUF",
			"%s holds no gguf files", repo)
	}
	return pickQuant(repo, ggufs, quant)
}

// auxiliaryGGUF reports whether a .gguf file is something other than the
// model's own weights.
//
// A repository ships more than the model. "mmproj-F16.gguf" is the
// multimodal projector — under a gigabyte beside a fifty-gigabyte model — and
// llama.cpp loads it alongside the weights, never instead of them. Because
// selection prefers the shortest matching name, the projector beat the model
// outright: a live probe resolved --quantization fp16 to mmproj-F16.gguf, which
// would have rented a 128 GB box and handed the engine a file that is not a
// model. Adapters and vocabulary-only files are the same class of mistake.
func auxiliaryGGUF(file string) bool {
	l := strings.ToLower(file[strings.LastIndex(file, "/")+1:])
	for _, marker := range []string{"mmproj", "lora", "adapter", "vocab", "projector"} {
		if strings.Contains(l, marker) {
			return true
		}
	}
	return false
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
	wanted := quantAliases(q)
	var candidates []string
	for _, f := range files {
		if isLaterShard(f) || auxiliaryGGUF(f) {
			continue
		}
		if q == "" {
			candidates = append(candidates, f)
			continue
		}
		// Compare the file's own quantisation tag first. Substring matching
		// alone conflates neighbours — "f16" is inside "bf16", so a request
		// for fp16 would take a BF16 file from a repository carrying both,
		// and which one it got would depend on filename length.
		if tag := strings.ToLower(quantTag(f)); tag != "" {
			for _, w := range wanted {
				if tag == w {
					candidates = append(candidates, f)
					break
				}
			}
			continue
		}
		lf := strings.ToLower(f)
		for _, w := range wanted {
			if strings.Contains(lf, w) {
				candidates = append(candidates, f)
				break
			}
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

// quantAliases returns the spellings a requested quantisation may appear
// under.
//
// The float formats have two names each and both are in common use: a
// repository writes "F16" where an operator, and the rest of LARRI, writes
// "fp16". Matching only the literal string reports "no fp16 quantisation"
// about a repository whose file listing plainly shows F16 — a refusal the
// operator cannot act on because there is nothing wrong with what they asked.
func quantAliases(q string) []string {
	switch q {
	case "fp16", "f16", "float16", "half":
		return []string{"f16"}
	case "fp32", "f32", "float32":
		return []string{"f32"}
	case "bf16", "bfloat16":
		return []string{"bf16"}
	}
	return []string{q}
}

// quantTag picks the quantisation out of a GGUF filename.
//
// By shape, not by position. Splitting on the last dot assumes names like
// "model.Q4_K_M.gguf", and breaks on the equally common
// "Qwen3.6-27B-Q4_K_M.gguf" — where the dot belongs to the model's version —
// reporting the quantisation as "6-27B-Q4_K_M". An operator reading that has
// been handed a string they cannot pass back.
//
// GGUF quantisation names are a small, well-defined family: Q or IQ followed
// by a digit, or one of the float formats.
func quantTag(file string) string {
	base := strings.TrimSuffix(file[strings.LastIndex(file, "/")+1:], ".gguf")
	toks := strings.FieldsFunc(base, func(r rune) bool { return r == '-' || r == '.' })
	for i := len(toks) - 1; i >= 0; i-- {
		t := toks[i]
		u := strings.ToUpper(t)
		switch u {
		case "F16", "F32", "BF16", "FP16", "FP32":
			return u
		}
		// Q4_K_M, Q8_0, IQ4_XS … the underscore-joined remainder travels
		// with the leading token because FieldsFunc does not split on it.
		if len(u) >= 2 && (u[0] == 'Q' || strings.HasPrefix(u, "IQ")) {
			d := u[1:]
			if strings.HasPrefix(u, "IQ") {
				d = u[2:]
			}
			if d != "" && d[0] >= '0' && d[0] <= '9' {
				return u
			}
		}
	}
	return ""
}

// quantsIn summarises what a repository offers, so a miss is actionable.
func quantsIn(files []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		if auxiliaryGGUF(f) {
			continue
		}
		tag := quantTag(f)
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

// suggestGGUFRepo names a GGUF conversion of the same model, or "" when there
// is nothing worth naming.
//
// Advisory: it runs only on a path that has already failed, and a search that
// errors or finds nothing leaves the original message alone. It borrows the
// sizing package's finder so that "is this the same model", "does it ship
// weights we will load", and "is this publication actually used" are answered
// the same way here as they are for vLLM — the rules matter more than the
// engine asking.
func suggestGGUFRepo(ctx context.Context, repo string, token secret.Secret) string {
	r := sizing.NewHFResolver(token)
	vars, err := r.FindQuantised(ctx, repo, func(q string) bool { return q == "gguf" })
	if err != nil || len(vars) == 0 {
		return ""
	}
	// Smallest first is the finder's order, which is right when the question
	// is what to download. Here the question is which repository to name, so
	// prefer the one most people use.
	best := vars[0]
	for _, v := range vars[1:] {
		if v.Downloads > best.Downloads {
			best = v
		}
	}
	return best.Ref
}

// ggufSizes lists the quantisations a repository carries with their sizes,
// first shard only.
func ggufSizes(info hfModelInfo) map[string]uint64 {
	out := map[string]uint64{}
	for _, s := range info.Siblings {
		f := s.RFilename
		if !strings.HasSuffix(strings.ToLower(f), ".gguf") || auxiliaryGGUF(f) {
			continue
		}
		tag := quantTag(f)
		if tag == "" {
			continue
		}
		// Shards belong to one quantisation, so they add up rather than
		// compete: a BF16 split across two files is the size of both.
		out[tag] += s.Size
	}
	return out
}

// adviseSmallerQuant reports the quantisations worth having instead of the
// chosen one.
//
// Only meaningfully smaller ones, and only the two nearest, because a list of
// twenty is a list nobody reads. The chosen size is the comparison, so the
// saving is stated rather than implied.
func adviseSmallerQuant(repo, chosen string, sizes map[string]uint64) []string {
	chosenSize, ok := sizes[chosen]
	if !ok || chosenSize == 0 {
		return nil
	}
	type opt struct {
		tag  string
		size uint64
	}
	var smaller []opt
	for tag, sz := range sizes {
		if sz == 0 || tag == chosen {
			continue
		}
		// A quarter off is the point at which the download time changes
		// enough to be worth an operator's attention.
		if float64(sz) <= 0.75*float64(chosenSize) {
			smaller = append(smaller, opt{tag, sz})
		}
	}
	if len(smaller) == 0 {
		return nil
	}
	// Largest of the small ones first: the nearest alternative is the one
	// that gives up least quality for the saving.
	sort.Slice(smaller, func(i, j int) bool { return smaller[i].size > smaller[j].size })
	if len(smaller) > 2 {
		smaller = smaller[:2]
	}
	var out []string
	for _, o := range smaller {
		out = append(out, fmt.Sprintf("%s carries %s at %.1f GB against %s at %.1f GB (%.0f%% less to fetch) — --quantization %s",
			repo, o.tag, float64(o.size)/1e9, chosen, float64(chosenSize)/1e9,
			100*(1-float64(o.size)/float64(chosenSize)), o.tag))
	}
	return out
}

// fetchGGUFListing reads a repository's file listing, with sizes.
func fetchGGUFListing(ctx context.Context, repo string, token secret.Secret) (hfModelInfo, error) {
	var info hfModelInfo
	// blobs=true so file sizes come back with the listing. They cost nothing
	// extra here and are what lets a smaller quantisation be offered with a
	// number attached rather than as a vague suggestion.
	url := "https://huggingface.co/api/models/" + repo + "?blobs=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return info, err
	}
	if !token.Empty() {
		req.Header.Set("Authorization", "Bearer "+token.Reveal())
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return info, errs.Newf(errs.ClassProviderTransient, "llamacpp.ResolveGGUF",
			"list %s: %v", repo, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return info, errs.Newf(errs.ClassModelFailure, "llamacpp.ResolveGGUF",
			"no repository %s", repo)
	}
	if resp.StatusCode != http.StatusOK {
		return info, errs.Newf(errs.ClassProviderTransient, "llamacpp.ResolveGGUF",
			"list %s: http %d", repo, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&info); err != nil {
		return info, errs.Newf(errs.ClassProviderTransient, "llamacpp.ResolveGGUF",
			"decode %s: %v", repo, err)
	}
	return info, nil
}

// AdviseModel reports a cheaper way to fetch the same model.
//
// The chosen quantisation works; it is simply often four times larger than
// one sitting in the same repository, and that difference is paid in billed
// download time on every rental. Advisory only, and silent on any error —
// nothing here may interfere with a bring-up.
func (r *Runtime) AdviseModel(ctx context.Context, spec core.ModelSpec) []string {
	repo := RepoOf(spec.Ref)
	info, err := fetchGGUFListing(ctx, repo, r.hfToken)
	if err != nil {
		return nil
	}
	var files []string
	for _, sib := range info.Siblings {
		if strings.HasSuffix(strings.ToLower(sib.RFilename), ".gguf") {
			files = append(files, sib.RFilename)
		}
	}
	chosen, err := pickQuant(repo, files, spec.Quantization)
	if err != nil {
		return nil
	}
	return adviseSmallerQuant(repo, quantTag(chosen), ggufSizes(info))
}
