// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package rank selects which offer to rent.
//
// Criteria are a hard filter; among everything that survives, LARRI takes the
// cheapest offer that passes the safety floors (§8). That is the product goal
// stated plainly — the operator says what they need, and LARRI finds the least
// expensive hardware that provides it.
//
// The floors exist because selecting on price alone walks straight toward
// whatever a host fishing for prompts and tokens would list. Live Vast data
// for one GPU model: 208 offers from $0.135 to $8.00. The cheapest is either a
// bargain on tired hardware or a trap, and nothing in the price distinguishes
// them, so reliability and distance from the class median do.
package rank

import (
	"fmt"
	"sort"

	"go.sovrenix.com/larri/internal/core"
)

// Policy configures the safety floors.
type Policy struct {
	// ReliabilityFloor excludes hosts below this provider-reported score.
	ReliabilityFloor float64

	// OutlierFactor excludes offers priced below median/OutlierFactor for
	// their GPU class.
	OutlierFactor float64

	// MinClassSample is how many offers a GPU class needs before the outlier
	// test means anything. A class with four listings has no distribution to
	// be an outlier in.
	MinClassSample int

	// ColdStartBytes is what this rig must download before it can serve: the
	// runtime image, then the weights. Zero falls back to ranking on the
	// hourly rate alone.
	ColdStartBytes uint64

	// SessionHours is how long the operator is expected to use the rig, and
	// it is what turns an hourly rate into a bill.
	//
	// The cheapest hourly rate is routinely the most expensive way to get a
	// working endpoint, because the download is billed at that rate too. A
	// measured pair from one market: $0.216/hr on a 1347 Mbps link reaches
	// ready in six minutes; $0.109/hr on a 68.7 Mbps link needs nearly two
	// hours of billed downloading first. The cheap host costs more for any
	// session under six hours, and is unusable for the first two of them.
	SessionHours float64
}

// DefaultPolicy is what runs when the operator has configured nothing.
func DefaultPolicy() Policy {
	return Policy{ReliabilityFloor: 0.90, OutlierFactor: 3.0, MinClassSample: 8,
		SessionHours: 1}
}

// coldStartHours is how long this offer spends downloading before it serves.
//
// An offer whose link speed is unreported gets the market's median rather
// than a zero, so a provider that publishes nothing neither wins by default
// nor loses by default.
func coldStartHours(o core.Offer, bytes uint64, medianMbps float64) float64 {
	if bytes == 0 {
		return 0
	}
	mbps := o.NetDownMbps
	if mbps <= 0 {
		mbps = medianMbps
	}
	if mbps <= 0 {
		return 0
	}
	return float64(bytes) * 8 / (mbps * 1e6) / 3600
}

// sessionCost is what an operator actually pays for this offer: the billed
// download plus the billed use.
func sessionCost(o core.Offer, p Policy, medianMbps float64) float64 {
	hours := p.SessionHours
	if hours <= 0 {
		hours = 1
	}
	return (coldStartHours(o, p.ColdStartBytes, medianMbps) + hours) * o.PriceHr
}

// medianNetMbps is the market's typical link, used to stand in for offers
// that do not report one.
func medianNetMbps(offers []core.Offer) float64 {
	var speeds []float64
	for _, o := range offers {
		if o.NetDownMbps > 0 {
			speeds = append(speeds, o.NetDownMbps)
		}
	}
	if len(speeds) == 0 {
		return 0
	}
	sort.Float64s(speeds)
	return speeds[len(speeds)/2]
}

// Reason is why an offer was not selected. Typed, because FR-SRCH-03 requires
// selection to be inspectable and an operator asking "why not the cheap one"
// deserves the actual reason rather than a score.
type Reason string

const (
	ReasonEligible      Reason = ""
	ReasonVRAM          Reason = "insufficient-vram"
	ReasonReliability   Reason = "reliability-below-floor"
	ReasonPriceOutlier  Reason = "price-outlier"
	ReasonInterruptible Reason = "interruptible-not-permitted"
	ReasonMaxPrice      Reason = "above-max-price"
	ReasonRegion        Reason = "region-blocked"
	ReasonCostlier      Reason = "costlier-than-selection"
)

// Candidate is one offer with the verdict on it.
type Candidate struct {
	Offer       core.Offer
	Reason      Reason
	Detail      string  // human-readable, evidence-bearing
	ClassMedian float64 // median price for this GPU class, 0 if unknown
}

// Eligible reports whether this offer could have been selected.
func (c Candidate) Eligible() bool {
	return c.Reason == ReasonEligible || c.Reason == ReasonCostlier
}

// Result is the outcome of a selection.
type Result struct {
	Selected   *Candidate
	Candidates []Candidate // every offer considered, cheapest first
}

// Excluded returns the offers that were filtered out, cheapest first. These
// are what the operator sees when they ask why the cheap one was not taken.
func (r Result) Excluded() []Candidate {
	var out []Candidate
	for _, c := range r.Candidates {
		if !c.Eligible() {
			out = append(out, c)
		}
	}
	return out
}

// FitFunc reports whether an offer can hold the model, and why not if it
// cannot. Supplied by the caller so this package does not depend on sizing.
type FitFunc func(core.Offer) (ok bool, detail string)

// Select applies the filters and returns the cheapest survivor.
//
// Fit is a filter rather than a scoring term. It answers one question — will
// the model run — and once answered has no further business competing with
// price. An 80 GB card serving a 19 GB model is wasteful, but if it is the
// cheapest thing that works then it is the right answer.
func Select(offers []core.Offer, c core.Criteria, fits FitFunc, p Policy) Result {
	medians := classMedians(offers, p.MinClassSample)

	cands := make([]Candidate, 0, len(offers))
	for _, o := range offers {
		cand := Candidate{Offer: o, ClassMedian: medians[o.GPUModel]}
		cand.Reason, cand.Detail = classify(o, c, fits, p, medians)
		cands = append(cands, cand)
	}

	// Cheapest *to a working endpoint* first, then deterministic tie-breaks,
	// so the same market produces the same choice twice — which is what makes
	// a selection reproducible in a bug report.
	//
	// Sorting on the hourly rate alone is what kept choosing hosts that could
	// not deliver: the download is billed at that rate, so a slow link turns
	// the cheapest listing into the dearest rig and leaves it unusable for
	// hours first.
	median := medianNetMbps(offers)
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i].Offer, cands[j].Offer
		ca, cb := sessionCost(a, p, median), sessionCost(b, p, median)
		if ca != cb {
			return ca < cb
		}
		if a.PriceHr != b.PriceHr {
			return a.PriceHr < b.PriceHr
		}
		if a.Reliability != b.Reliability {
			return a.Reliability > b.Reliability
		}
		return a.OfferID < b.OfferID
	})

	res := Result{Candidates: cands}
	for i := range res.Candidates {
		if res.Candidates[i].Reason == ReasonEligible {
			res.Selected = &res.Candidates[i]
			break
		}
	}
	if res.Selected != nil {
		// Everything eligible but pricier is marked so, rather than left
		// looking like it failed a check it passed.
		for i := range res.Candidates {
			c := &res.Candidates[i]
			if c != res.Selected && c.Reason == ReasonEligible {
				c.Reason = ReasonCostlier
				c.Detail = fmt.Sprintf("$%.3f against the selected $%.3f to serve %gh",
					sessionCost(c.Offer, p, median),
					sessionCost(res.Selected.Offer, p, median), sessionHours(p))
			}
		}
	}
	return res
}

func classify(o core.Offer, c core.Criteria, fits FitFunc, p Policy,
	medians map[string]float64) (Reason, string) {

	if !c.Interruptible.Permits(o.Interruptible) {
		return ReasonInterruptible, "interruptible offers are opt-in"
	}
	if c.MaxPriceHr > 0 && o.PriceHr > c.MaxPriceHr {
		return ReasonMaxPrice, fmt.Sprintf("$%.3f/hr above the $%.2f/hr ceiling",
			o.PriceHr, c.MaxPriceHr)
	}
	if fits != nil {
		if ok, detail := fits(o); !ok {
			return ReasonVRAM, detail
		}
	}
	// The floor applies only where there is a score to apply it to. A
	// provider that reports none is not a provider whose every host has
	// failed — it is one that lists GPU types rather than named machines, and
	// filtering its whole catalogue out would present an empty market as the
	// answer.
	if p.ReliabilityFloor > 0 && o.HasReliability() && o.Reliability < p.ReliabilityFloor {
		return ReasonReliability, fmt.Sprintf("reliability %.2f below floor %.2f",
			o.Reliability, p.ReliabilityFloor)
	}
	if med, ok := medians[o.GPUModel]; ok && p.OutlierFactor > 1 && o.PriceHr > 0 {
		if o.PriceHr*p.OutlierFactor < med {
			return ReasonPriceOutlier, fmt.Sprintf(
				"%.1f× below the %s median of $%.3f/hr",
				med/o.PriceHr, o.GPUModel, med)
		}
	}
	return ReasonEligible, ""
}

// classMedians computes a median price per GPU model, for classes with enough
// listings to have a distribution.
//
// Median rather than mean, and the live data is why: RTX 4090 offers ran
// $0.135 to $8.002 with a mean of $0.951, which describes almost nothing — the
// mean is dragged upward by a tail of listings priced in H100 territory. A rule
// written against it would flag a wide band of legitimately cheap offers while
// missing what it was written for.
func classMedians(offers []core.Offer, minSample int) map[string]float64 {
	if minSample <= 0 {
		minSample = 1
	}
	byClass := map[string][]float64{}
	for _, o := range offers {
		if o.PriceHr > 0 {
			byClass[o.GPUModel] = append(byClass[o.GPUModel], o.PriceHr)
		}
	}
	out := make(map[string]float64, len(byClass))
	for model, prices := range byClass {
		if len(prices) < minSample {
			continue // no distribution to be an outlier in
		}
		sort.Float64s(prices)
		out[model] = median(prices)
	}
	return out
}

func median(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// sessionHours is SessionHours with its default applied.
func sessionHours(p Policy) float64 {
	if p.SessionHours <= 0 {
		return 1
	}
	return p.SessionHours
}
