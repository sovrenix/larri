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
	base, err := Plan(Request{Spec: req.Spec, Facts: req.Facts, Concurrency: req.Concurrency})
	s := Shortfall{
		Model:      req.Spec.Ref,
		Quant:      req.Spec.Quantization,
		ContextLen: req.Spec.ContextLen,
	}
	if err == nil {
		s.RequiredB = base.RequiredVRAMBytes
	}

	sorted := append([]core.Offer(nil), candidates...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].PriceHr < sorted[j].PriceHr })
	for i := range sorted {
		o := sorted[i]
		avail := uint64(o.VRAMTotalGB()) * GiB
		if s.Best == nil || avail > uint64(s.Best.VRAMTotalGB())*GiB {
			s.Best = &sorted[i]
		}
		if s.CheapestFit == nil && avail >= s.RequiredB {
			s.CheapestFit = &sorted[i]
		}
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
		p, err := Plan(Request{Spec: alt, Facts: req.Facts, Concurrency: req.Concurrency})
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
		p, err := Plan(Request{Spec: alt, Facts: req.Facts, Concurrency: req.Concurrency})
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
		avail := uint64(s.Best.VRAMTotalGB()) * GiB
		fmt.Fprintf(&b, "  Best matching offer: %s %dGB ($%.2f/hr)",
			s.Best.GPUModel, s.Best.VRAMTotalGB(), s.Best.PriceHr)
		if avail < s.RequiredB {
			fmt.Fprintf(&b, " — %s short", HumanBytes(s.RequiredB-avail))
		}
		b.WriteString(".\n")
	} else {
		b.WriteString("  No offer satisfied the other criteria, so none could be compared.\n")
	}

	switch {
	case s.CheapestFit != nil:
		fmt.Fprintf(&b, "  Cheapest offer that would fit: %s %dGB ($%.2f/hr).\n",
			s.CheapestFit.GPUModel, s.CheapestFit.VRAMTotalGB(), s.CheapestFit.PriceHr)
	case s.Best != nil:
		// Silence here would read as "we did not look". Saying it plainly is
		// what turns a rejection into a decision the operator can act on.
		b.WriteString("  No offer on the table has enough VRAM at this size.\n")
	}

	var tries []string
	for _, sg := range s.Suggestions {
		if sg.Fits {
			tries = append(tries, fmt.Sprintf("%s (~%s)", sg.Flag, HumanBytes(sg.RequiredB)))
		}
	}
	if len(tries) > 0 {
		fmt.Fprintf(&b, "  Try: %s.\n", strings.Join(tries, " or "))
	}
	return b.String()
}

func humanCtx(n int) string {
	if n >= 1024 && n%1024 == 0 {
		return fmt.Sprintf("%dk", n/1024)
	}
	return fmt.Sprintf("%d", n)
}
