// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sizing

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// MinVariantDownloads is how much use an unfamiliar publication needs before
// LARRI will suggest it.
//
// Not a security control — downloads are gameable — but it separates a
// quantisation people actually run from one uploaded yesterday by an account
// nobody has heard of, which is the difference between a useful suggestion
// and an invitation to load a stranger's weights.
const MinVariantDownloads = 100

// Variant is a quantised publication of a model.
//
// It exists because the download is most of what a cold start costs, and
// quantisation is the only lever that changes the number of bytes rather than
// the speed they arrive at. Measured on one model: Qwen3.6-27B is 55.6 GB at
// bf16 and 19.0 GB at int4 — nearly three times less to fetch, and small
// enough to fit hardware that costs a fraction as much.
type Variant struct {
	Ref         string
	Quant       string // awq, gptq, fp8, int4 …
	WeightBytes uint64
	Downloads   int

	// SameOwner reports whether this comes from the account that published
	// the original. A quantisation is a re-upload of someone else's weights
	// by a third party, which is a supply-chain question, not merely a
	// filename difference — so who published it travels with the suggestion.
	SameOwner bool
}

// SavingOver reports the fraction of bytes this variant avoids.
func (v Variant) SavingOver(baseBytes uint64) float64 {
	if baseBytes == 0 || v.WeightBytes >= baseBytes {
		return 0
	}
	return 1 - float64(v.WeightBytes)/float64(baseBytes)
}

// quantMarkers maps a marker that may appear in a repo name or tag to the
// canonical quantisation name.
//
// Container formats come first, because they decide whether an engine can
// load the file at all, while a precision does not: "mlx-community/…-4bit" is
// an int4 model that vLLM cannot read, and matching its precision before its
// format would offer an Apple-only build to a CUDA engine.
var quantMarkers = []struct{ marker, quant string }{
	{"gguf", "gguf"},
	{"mlx", "mlx"},
	{"awq", "awq"},
	{"gptq", "gptq"},
	{"autoround", "int4"},
	{"nvfp4", "nvfp4"},
	{"mxfp4", "mxfp4"},
	{"fp8", "fp8"},
	{"int8", "int8"},
	{"8bit", "int8"},
	{"int4", "int4"},
	{"4bit", "int4"},
}

// DetectQuant names the quantisation a repo advertises, or "" for none.
func DetectQuant(ref string, tags []string) string {
	hay := strings.ToLower(ref)
	for _, t := range tags {
		hay += " " + strings.ToLower(t)
	}
	for _, m := range quantMarkers {
		if strings.Contains(hay, m.marker) {
			return m.quant
		}
	}
	return ""
}

// BaseName reduces a model reference to the part a sibling would share.
//
// "Qwen/Qwen3.6-27B" becomes "Qwen3.6-27B", which is what a quantiser puts in
// their own repo name.
func BaseName(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
	return ref
}

// looksLikeSameModel guards against a search returning a different model that
// merely shares a prefix — a fine-tune, a distinct size, or an unrelated
// project. The base name must appear intact.
func looksLikeSameModel(candidate, base string) bool {
	return strings.Contains(strings.ToLower(BaseName(candidate)), strings.ToLower(base))
}

type hfSearchResult struct {
	ID        string   `json:"id"`
	Downloads int      `json:"downloads"`
	Tags      []string `json:"tags"`
}

type hfBlobs struct {
	Siblings []struct {
		Name string `json:"rfilename"`
		Size uint64 `json:"size"`
	} `json:"siblings"`
}

// weightBytes totals the weight files, and reports whether any are
// safetensors.
//
// FR-SEC: pickle checkpoints execute arbitrary code on load, on the machine
// holding the operator's Hugging Face token. A variant that ships only .bin
// is not a saving worth having.
func weightBytes(b hfBlobs) (total uint64, safetensors bool) {
	for _, s := range b.Siblings {
		switch {
		case strings.HasSuffix(s.Name, ".safetensors"):
			total += s.Size
			safetensors = true
		case strings.HasSuffix(s.Name, ".gguf"), strings.HasSuffix(s.Name, ".bin"):
			total += s.Size
		}
	}
	return total, safetensors
}

// FindQuantised searches for quantised publications of ref that accept
// approves of.
//
// It is advisory and must never fail a bring-up: the caller is expected to
// ignore the error and carry on. The whole search is deadline-bounded, and
// only a handful of candidates are measured, because each measurement is a
// separate request and this runs before the operator has agreed to spend
// anything.
func (h *HFResolver) FindQuantised(ctx context.Context, ref string, accept func(quant string) bool) ([]Variant, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	base := BaseName(ref)
	owner, _, _ := strings.Cut(ref, "/")
	body, err := h.get(ctx, "/api/models?search="+base+"&limit=50&sort=downloads&direction=-1")
	if err != nil {
		return nil, err
	}
	var found []hfSearchResult
	if err := json.Unmarshal(body, &found); err != nil {
		return nil, fmt.Errorf("sizing: variant search: %w", err)
	}

	type cand struct {
		res   hfSearchResult
		quant string
	}
	var cands []cand
	for _, r := range found {
		if strings.EqualFold(r.ID, ref) || !looksLikeSameModel(r.ID, base) {
			continue
		}
		q := DetectQuant(r.ID, r.Tags)
		if q == "" || !accept(q) {
			continue
		}
		cands = append(cands, cand{r, q})
	}
	// Most-downloaded first, then measure only the top few: each measurement
	// is a request, and this runs before anything has been agreed to.
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].res.Downloads > cands[j].res.Downloads })
	const measure = 6
	if len(cands) > measure {
		cands = cands[:measure]
	}

	var (
		mu  sync.Mutex
		out []Variant
		wg  sync.WaitGroup
	)
	for _, c := range cands {
		wg.Add(1)
		go func(c cand) {
			defer wg.Done()
			b, err := h.get(ctx, "/api/models/"+c.res.ID+"?blobs=true")
			if err != nil {
				return
			}
			var blobs hfBlobs
			if json.Unmarshal(b, &blobs) != nil {
				return
			}
			total, safe := weightBytes(blobs)
			if total == 0 || !safe {
				return // unmeasurable, or pickle-only
			}
			vOwner, _, _ := strings.Cut(c.res.ID, "/")
			// A quantisation is a third party's re-upload of someone else's
			// weights, executed on hardware holding the operator's HF token.
			// Smallest-wins alone would recommend whatever obscure account
			// happened to squeeze hardest, so an unknown publication has to
			// be in actual use before LARRI names it.
			if !strings.EqualFold(vOwner, owner) && c.res.Downloads < MinVariantDownloads {
				return
			}
			mu.Lock()
			out = append(out, Variant{
				Ref: c.res.ID, Quant: c.quant, WeightBytes: total,
				Downloads: c.res.Downloads,
				SameOwner: strings.EqualFold(vOwner, owner),
			})
			mu.Unlock()
		}(c)
	}
	wg.Wait()

	// Smallest first — the point is bytes not fetched. Ties break toward the
	// more widely used publication, then by name for determinism.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].WeightBytes != out[j].WeightBytes {
			return out[i].WeightBytes < out[j].WeightBytes
		}
		if out[i].Downloads != out[j].Downloads {
			return out[i].Downloads > out[j].Downloads
		}
		return out[i].Ref < out[j].Ref
	})
	return out, nil
}
