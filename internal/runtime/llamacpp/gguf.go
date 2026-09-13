// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package llamacpp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.sovrenix.com/larri/internal/sizing"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/secret"
)

// GGUFFile reports which file in a repository to download.
//
// A repository is not a model here: a GGUF repo holds one file per quantisation
// — a dozen or more — and downloading the wrong one costs the whole transfer at
// rented-GPU prices before anything reveals the mistake. So this resolves
// rather than guesses.
//
// Two forms are accepted. A ref naming a file directly ("owner/repo/x.gguf")
// is checked against the listing. Otherwise the repository is listed and the
// file is matched against the requested quantisation, which is the only
// reliable way: naming conventions across GGUF publishers agree on almost
// nothing except that the quantisation appears somewhere in the name.
func GGUFFile(spec core.ModelSpec) (string, error) {
	if f := explicitFile(spec.Ref); f != "" {
		return f, nil
	}
	return "", errs.Newf(errs.ClassModelFailure, "llamacpp.GGUFFile",
		"gguf file for %s not resolved: call ResolveGGUF before bootstrap", spec.Ref)
}

// explicitFile returns the file's path within its repository when a ref
// names one outright — directory included, since that is where it downloads
// from.
func explicitFile(ref string) string {
	_, file := sizing.SplitRef(ref)
	return file
}

// RepoOf strips a trailing file path from a ref, leaving the repository.
func RepoOf(ref string) string {
	repo, _ := sizing.SplitRef(ref)
	return repo
}

type hfModelInfo struct {
	Siblings []struct {
		RFilename string `json:"rfilename"`
		Size      uint64 `json:"size"`
	} `json:"siblings"`
}

// Weights is a resolved GGUF: which file to fetch first, and how large the
// whole set is.
//
// The size is carried because sizing should measure rather than estimate. The
// listing already returns it — the request asks for blobs — so the figure that
// decides how much VRAM to rent costs nothing extra to obtain, and it is the
// weights themselves rather than a parameter count multiplied by a table of
// averages. Zero means the repository did not publish sizes.
type Weights = runtime.Weights

// ResolveGGUF picks the file matching the requested quantisation.
//
// It runs locally, before anything is rented, so a repository that does not
// carry the requested quantisation is a line of output rather than a bill. The
// error lists what the repo *does* carry, because "not found" without the
// alternatives leaves the operator to go and look it up themselves.
func ResolveGGUF(ctx context.Context, ref, quant string, token secret.Secret) (Weights, error) {
	repo, named := sizing.SplitRef(ref)
	info, err := fetchGGUFListing(ctx, repo, token)
	if err != nil {
		return Weights{}, err
	}

	var ggufs []string
	sizes := map[string]uint64{}
	for _, s := range info.Siblings {
		if strings.HasSuffix(strings.ToLower(s.RFilename), ".gguf") {
			ggufs = append(ggufs, s.RFilename)
			sizes[s.RFilename] = s.Size
		}
	}
	if len(ggufs) == 0 {
		// The commonest way to reach this: an operator names the original
		// weights, which are safetensors, because that is the repository the
		// model is known by. A GGUF conversion almost always exists under a
		// different account, and naming it turns a dead end into one edit.
		if alt := suggestGGUFRepo(ctx, repo, token); alt != "" {
			return Weights{}, errs.Newf(errs.ClassModelFailure, "llamacpp.ResolveGGUF",
				"%s holds no gguf files: try %s", repo, alt)
		}
		return Weights{}, errs.Newf(errs.ClassModelFailure, "llamacpp.ResolveGGUF",
			"%s holds no gguf files", repo)
	}
	if named != "" {
		return namedFile(repo, named, quant, ggufs, sizes)
	}
	file, err := pickQuant(repo, ggufs, quant)
	if err != nil {
		return Weights{}, err
	}
	return Weights{File: file, Bytes: shardBytes(file, sizes), Quantization: quant}, nil
}

// namedFile checks a file the operator named against the repository listing.
//
// A named file used to be taken at its word, with no listing fetched. That
// left it unmeasured, so sizing fell back to its estimate; a typo surfaced as
// a failed download on a rented host; and nothing noticed a name that was a
// draft head, or a quantisation other than the one the rig would be recorded
// and sized as.
func namedFile(repo, file, quant string, ggufs []string, sizes map[string]uint64) (Weights, error) {
	// llama.cpp is pointed at part one and finds the rest itself, so naming
	// any part names the set.
	first := ShardFiles(file)[0]
	if _, ok := sizes[first]; !ok {
		return Weights{}, errs.Newf(errs.ClassModelFailure, "llamacpp.ResolveGGUF",
			"%s has no file %s; it carries: %s", repo, file, strings.Join(quantsIn(ggufs), ", "))
	}
	if kind := auxiliaryKind(first); kind != "" {
		return Weights{}, errs.Newf(errs.ClassModelFailure, "llamacpp.ResolveGGUF",
			"%s is %s, not the model; %s carries: %s",
			file, kind, repo, strings.Join(quantsIn(ggufs), ", "))
	}
	tag := quantTag(first)
	if q := strings.ToLower(strings.TrimSpace(quant)); q != "" && tag != "" &&
		!matchesQuant(first, quantAliases(q)) {
		return Weights{}, errs.Newf(errs.ClassModelFailure, "llamacpp.ResolveGGUF",
			"%s carries %s, not %s", file, tag, quant)
	}
	if tag == "" {
		tag = quant
	}
	return Weights{File: first, Bytes: shardBytes(first, sizes), Quantization: tag}, nil
}

// shardBytes totals a quantisation across its shards.
//
// All of them or none of them: a partial total is worse than no total,
// because it would be believed. A repository that publishes sizes for some
// files and not others is not one to size a rental against, so the estimate
// takes over instead.
func shardBytes(first string, sizes map[string]uint64) uint64 {
	var total uint64
	for _, sh := range ShardFiles(first) {
		b, ok := sizes[sh]
		if !ok || b == 0 {
			return 0
		}
		total += b
	}
	return total
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
	// "mtp" is the multi-token-prediction draft head, and it is the same
	// mistake as mmproj with a worse disguise: it carries the quantisation
	// tag in its own name, so a repository publishing both matched it on
	// exactly the string the operator asked for. A live dry run against
	// unsloth/Qwen3.8-Flash-Next-GGUF resolved --quantization Q4_K_M to a
	// one-gigabyte draft head and planned to rent a 192 GB box to serve it,
	// because shortest-name preference then ranked it above the real weights.
	return auxiliaryKind(l) != ""
}

// auxiliaryKind names what a non-model GGUF actually is, or "" for weights.
//
// Named rather than merely detected, because the operator can see the file:
// the Hugging Face page for unsloth/Qwen3.8-Flash-Next-GGUF offers a download
// called mtp-…-Q4_K_M.gguf, so "this repository has no Q4_K_M" reads as a bug
// in LARRI. It carries the quantisation in its name and is 2.6 GB against
// 103.7 GB of real Q4-class weights.
func auxiliaryKind(file string) string {
	l := strings.ToLower(file[strings.LastIndex(file, "/")+1:])
	for _, m := range []struct{ marker, kind string }{
		{"mmproj", "a multimodal projector"},
		{"projector", "a multimodal projector"},
		{"mtp", "a multi-token-prediction draft head"},
		{"lora", "a LoRA adapter"},
		{"adapter", "an adapter"},
		{"vocab", "a vocabulary-only file"},
	} {
		if strings.Contains(l, m.marker) {
			return m.kind
		}
	}
	return ""
}

// matchesQuant reports whether a file carries one of the spellings asked for.
//
// The file's own quantisation tag is compared first. Substring matching alone
// conflates neighbours — "f16" is inside "bf16", so a request for fp16 would
// take a BF16 file from a repository carrying both, and which one it got
// would depend on filename length.
func matchesQuant(file string, wanted []string) bool {
	if tag := strings.ToLower(quantTag(file)); tag != "" {
		for _, w := range wanted {
			if tag == w {
				return true
			}
		}
		return false
	}
	lf := strings.ToLower(file)
	for _, w := range wanted {
		if strings.Contains(lf, w) {
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

	q := strings.ToLower(strings.TrimSpace(quant))
	wanted := quantAliases(q)
	var candidates []string
	// Files that match what was asked for but are not the model. Kept so a
	// refusal can name the file the operator is looking at rather than deny
	// it exists.
	var aux []string
	for _, f := range files {
		if laterShard(f) {
			continue
		}
		if auxiliaryGGUF(f) {
			if q != "" && matchesQuant(f, wanted) {
				aux = append(aux, f)
			}
			continue
		}
		if q == "" || matchesQuant(f, wanted) {
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
	if len(aux) > 0 {
		return "", errs.Newf(errs.ClassModelFailure, "llamacpp.ResolveGGUF",
			"%s has no %s weights: %s is %s, not the model; it carries: %s",
			repo, quant, aux[0], auxiliaryKind(aux[0]), strings.Join(quantsIn(files), ", "))
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
	// A repository's own directory name is a spelling an operator will type.
	// Unsloth publishes its dynamic quantisations under "UD-Q4_K_XL/", and
	// the tag inside the filename is "Q4_K_XL" — so the name on the folder
	// the operator was reading matched nothing at all.
	q = strings.TrimPrefix(q, "ud-")
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
	files := map[string]uint64{}
	for _, s := range info.Siblings {
		files[s.RFilename] = s.Size
	}
	out := map[string]uint64{}
	for _, s := range info.Siblings {
		f := s.RFilename
		if !strings.HasSuffix(strings.ToLower(f), ".gguf") || auxiliaryGGUF(f) || laterShard(f) {
			continue
		}
		tag := quantTag(f)
		if tag == "" {
			continue
		}
		// Shards belong to one quantisation, so they add up: a BF16 split
		// across two files is the size of both. Two publications of the same
		// quantisation do not — unsloth ships Q6_K of Llama-3.3-70B in two
		// folders, and adding everything tagged Q6_K priced it at 115.8 GB,
		// so the advice offered the larger Q8_0 as "35% less to fetch". Each
		// is one download; the smaller complete one is the size.
		total := shardBytes(f, files)
		if total == 0 {
			continue
		}
		if cur, ok := out[tag]; !ok || total < cur {
			out[tag] = total
		}
	}
	return out
}

// laterShard reports a part other than the first of a multi-part GGUF.
func laterShard(f string) bool {
	m := shardPattern.FindStringSubmatch(f[strings.LastIndex(f, "/")+1:])
	return m != nil && m[2] != "00001"
}

// adviseSmallerQuant reports the quantisations worth having instead of the
// chosen one.
//
// Only meaningfully smaller ones, and only the two nearest, because a list of
// twenty is a list nobody reads. The chosen size is the comparison, so the
// saving is stated rather than implied.
func adviseSmallerQuant(repo, chosen string, chosenSize uint64, sizes map[string]uint64) []string {
	if chosenSize == 0 {
		chosenSize = sizes[chosen]
	}
	if chosenSize == 0 {
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
// listingEndpoint is where repositories are listed from; a variable so tests
// can serve a listing without reaching Hugging Face.
var listingEndpoint = "https://huggingface.co"

func fetchGGUFListing(ctx context.Context, repo string, token secret.Secret) (hfModelInfo, error) {
	var info hfModelInfo
	// blobs=true so file sizes come back with the listing. They cost nothing
	// extra here and are what lets a smaller quantisation be offered with a
	// number attached rather than as a vague suggestion.
	url := listingEndpoint + "/api/models/" + repo + "?blobs=true"
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
	// The file already resolved is the comparison, at its own measured size:
	// re-picking from the listing could land on another publication of the
	// same quantisation, and a named file has no quantisation to pick by.
	chosen, size := r.weights.File, r.weights.Bytes
	if chosen == "" {
		if chosen, err = pickQuant(repo, files, spec.Quantization); err != nil {
			return nil
		}
	}
	return adviseSmallerQuant(repo, quantTag(chosen), size, ggufSizes(info))
}

// shardPattern matches llama.cpp's split-file naming: a 1-based index, the
// literal "-of-", and the total, both zero-padded to five digits.
var shardPattern = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})\.gguf$`)

// ShardFiles lists every part of a multi-part GGUF, in order.
//
// A single-file model returns itself, so callers need no special case.
//
// llama.cpp loads the remaining shards itself once it is pointed at the
// first, which is why resolution returns only that one — but *finding* them
// is the engine's job and *fetching* them is LARRI's, and for a while nothing
// did the second. Only shard one was downloaded, and the engine then failed
// on a missing tensor in a way that reads exactly like a corrupt transfer.
// Every model large enough to need more than one card is published in parts,
// so this is not an edge case for this engine; it is the case.
//
// The names are derived from the first shard rather than re-listed from the
// repository, because a ref that names a file outright never had a listing to
// derive them from and must work the same way.
func ShardFiles(first string) []string {
	m := shardPattern.FindStringSubmatch(first)
	if m == nil {
		return []string{first}
	}
	total, err := strconv.Atoi(m[3])
	if err != nil || total < 1 {
		return []string{first}
	}
	out := make([]string, 0, total)
	for i := 1; i <= total; i++ {
		out = append(out, fmt.Sprintf("%s-%05d-of-%s.gguf", m[1], i, m[3]))
	}
	return out
}

// localName is where a repository file lands on the host.
//
// Flattened to its base name, because GGUF repositories publish quantisations
// in directories — "UD-Q4_K_XL/model-00001-of-00004.gguf" — and the download
// created the model directory but never the one inside it, so curl failed to
// open the destination before a byte moved. Flattening also keeps a model's
// shards beside each other, which is where llama.cpp looks for them.
func localName(file string) string {
	return file[strings.LastIndex(file, "/")+1:]
}
