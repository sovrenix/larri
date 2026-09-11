// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sizing

import (
	"fmt"
	"sort"
	"strings"

	"go.sovrenix.com/larri/internal/core"
)

// Shortfall explains why nothing can serve a model, in the terms NFR-11
// requires: the VRAM needed, the VRAM found, and the cheapest offer that would
// fit.
//
// This is the most common error the tool produces, so it is treated as a
// first-class output rather than an error string. The operator's next action
// is almost always to quantise or to shorten the context, and the message
// exists to tell them which one would work and by how much.
type Shortfall struct {
	Model       string
	Quant       string
	ContextLen  int
	RequiredB   uint64
	Best        *core.Offer // best offer among those considered, may be nil
	BestVRAMB   uint64      // what the engine could have used on Best
	BestShards  int         // how many of Best's cards it could have used
	Considered  int         // how many offers were weighed, single- and multi-GPU
	CheapestFit *core.Offer // cheapest offer that would fit, may be nil
	Suggestions []Suggestion
}

// Suggestion is a change that would make the model fit.
type Suggestion struct {
	Flag      string // "--quantization q4_K_M"
	RequiredB uint64
	Fits      bool
}

// Analyse builds a Shortfall for a model that fits nothing on offer.
//
// candidates are the offers that satisfied the operator's other criteria; the
// caller has already filtered on price, region, and so on, so an empty list
// means the criteria were unsatisfiable before VRAM entered into it.
func Analyse(req Request, candidates []core.Offer) Shortfall {
	base, err := Plan(Request{Spec: req.Spec, Facts: req.Facts,
		Concurrency: req.Concurrency, WeightBytes: req.WeightBytes})
	s := Shortfall{
		Model:      req.Spec.Ref,
		Quant:      req.Spec.Quantization,
		ContextLen: req.Spec.ContextLen,
	}
	if err == nil {
		s.RequiredB = base.RequiredVRAMBytes
	}

	// Both figures are per offer, because both depend on how many of that
	// host's cards the engine can place the model on. Measuring every
	// candidate against one market-wide requirement is what let an eight-card
	// host be reported as the best available when the engine could only have
	// used four of them.
	sorted := append([]core.Offer(nil), candidates...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].PriceHr < sorted[j].PriceHr })
	s.Considered = len(sorted)
	var bestAvail uint64
	for i := range sorted {
		o := sorted[i]
		shards := req.shards(o.GPUCount)
		avail := ShardVRAM(o.VRAMPerGPUGB, shards)
		need := s.RequiredB
		if p, err := Plan(Request{
			Spec: req.Spec, Facts: req.Facts, Concurrency: req.Concurrency,
			GPUCount: shards, WeightBytes: req.WeightBytes,
		}); err == nil {
			need = p.RequiredVRAMBytes
		}
		if s.Best == nil || avail > bestAvail {
			s.Best, s.BestVRAMB, s.BestShards, bestAvail = &sorted[i], avail, shards, avail
		}
		if s.CheapestFit == nil && avail >= need {
			s.CheapestFit = &sorted[i]
		}
	}

	// Suggestions are measured against the largest VRAM the market actually
	// offered, not against nothing. With no target every alternative counts
	// as fitting, and the first one wins — which is how a 121.7 GB shortfall
	// was answered with "try --quantization q8_0 (~212.6 GB)", a suggestion
	// ninety gigabytes larger than the problem.
	req.AvailableVRAMBytes = bestAvail
	if s.Best != nil {
		req.GPUCount = req.shards(s.Best.GPUCount)
	}
	s.Suggestions = suggest(req)
	return s
}

// suggest tries the two levers an operator actually has — quantisation and
// context length — and reports what each would cost.
func suggest(req Request) []Suggestion {
	var out []Suggestion
	target := req.AvailableVRAMBytes

	for _, q := range []string{"q8_0", "q6_K", "q5_K_M", "q4_K_M"} {
		alt := req.Spec
		alt.Quantization = q
		// No WeightBytes here, deliberately. The measurement is of the file
		// the operator asked for; a different quantisation is a different
		// file, and carrying the figure across would price every alternative
		// as the one that already did not fit — which reads as "nothing
		// helps" whatever the repository actually offers.
		p, err := Plan(Request{Spec: alt, Facts: req.Facts,
			Concurrency: req.Concurrency, GPUCount: req.GPUCount})
		if err != nil {
			continue
		}
		fits := target == 0 || p.RequiredVRAMBytes <= target
		out = append(out, Suggestion{
			Flag: "--quantization " + q, RequiredB: p.RequiredVRAMBytes, Fits: fits,
		})
		if fits {
			break // the highest-quality quantisation that works is the useful one
		}
	}

	for _, c := range []int{32768, 16384, 8192, 4096} {
		if req.Spec.ContextLen <= c {
			continue
		}
		alt := req.Spec
		alt.ContextLen = c
		// The measurement does carry here: a shorter context is the same
		// weights, and the KV cache is what changes.
		p, err := Plan(Request{Spec: alt, Facts: req.Facts,
			Concurrency: req.Concurrency, GPUCount: req.GPUCount,
			WeightBytes: req.WeightBytes})
		if err != nil {
			continue
		}
		fits := target == 0 || p.RequiredVRAMBytes <= target
		out = append(out, Suggestion{
			Flag: fmt.Sprintf("--context %d", c), RequiredB: p.RequiredVRAMBytes, Fits: fits,
		})
		if fits {
			break
		}
	}
	return out
}

// String renders the message an operator sees before anything is spent.
func (s Shortfall) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "✗ %s", s.Model)
	if s.Quant != "" {
		fmt.Fprintf(&b, " @ %s", s.Quant)
	}
	if s.ContextLen > 0 {
		fmt.Fprintf(&b, ", %s context", humanCtx(s.ContextLen))
	}
	fmt.Fprintf(&b, " needs ~%s VRAM.\n", HumanBytes(s.RequiredB))

	if s.Best != nil {
		fmt.Fprintf(&b, "  Best matching offer: %s ($%.2f/hr)",
			s.Best.Hardware(), s.Best.PriceHr)
		if s.BestVRAMB < s.RequiredB {
			fmt.Fprintf(&b, " — %s short", HumanBytes(s.RequiredB-s.BestVRAMB))
		}
		// Without this the two figures on the line cannot be reconciled: a
		// host advertising 144GB reported 67.3 GB short of a 158.5 GB
		// requirement reads as an arithmetic bug until the shard degree is
		// named. The cards the engine cannot reach hold no part of the model.
		if s.BestShards > 0 && s.BestShards < s.Best.GPUCount {
			fmt.Fprintf(&b, " (the engine shards across %d of %d cards, %s usable)",
				s.BestShards, s.Best.GPUCount, HumanBytes(s.BestVRAMB))
		}
		b.WriteString(".\n")
	} else {
		b.WriteString("  No offer satisfied the other criteria, so none could be compared.\n")
	}

	switch {
	case s.CheapestFit != nil:
		fmt.Fprintf(&b, "  Cheapest offer that would fit: %s ($%.2f/hr).\n",
			s.CheapestFit.Hardware(), s.CheapestFit.PriceHr)
	case s.Best != nil:
		// Silence here would read as "we did not look". Saying it plainly is
		// what turns a rejection into a decision the operator can act on.
		// The count is named because multi-GPU hosts are in the list and were
		// considered: "no offer has enough VRAM" otherwise invites the reply
		// that two of them obviously would.
		fmt.Fprintf(&b, "  No offer on the table has enough VRAM at this size, "+
			"single or multi-GPU (%d considered, up to %s usable).\n",
			s.Considered, HumanBytes(s.BestVRAMB))
	}

	var tries []string
	for _, sg := range s.Suggestions {
		if sg.Fits {
			tries = append(tries, fmt.Sprintf("%s (~%s)", sg.Flag, HumanBytes(sg.RequiredB)))
		}
	}
	if len(tries) > 0 {
		fmt.Fprintf(&b, "  Try: %s.\n", strings.Join(tries, " or "))
	} else if s.Best != nil {
		// Every lever was tried and none of them reached. Saying nothing here
		// is the shrug the whole report exists to avoid.
		b.WriteString("  Neither a smaller quantisation nor a shorter context " +
			"brings it within reach of this market.\n")
	}
	return b.String()
}

func humanCtx(n int) string {
	if n >= 1024 && n%1024 == 0 {
		return fmt.Sprintf("%dk", n/1024)
	}
	return fmt.Sprintf("%d", n)
}
