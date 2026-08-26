// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package rank

import (
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/core"
)

func offer(id, model string, price, rel float64, vram int) core.Offer {
	return core.Offer{
		Provider: "vastai", OfferID: id, GPUModel: model,
		GPUCount: 1, VRAMPerGPUGB: vram, PriceHr: price, Reliability: rel,
	}
}

// needs builds a FitFunc requiring at least gb of total VRAM.
func needs(gb int) FitFunc {
	return func(o core.Offer) (bool, string) {
		if o.VRAMTotalGB() >= gb {
			return true, ""
		}
		return false, "needs " + itoa(gb) + " GB, offer has " + itoa(o.VRAMTotalGB())
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// The product goal, stated plainly: among everything meeting the criteria,
// take the least expensive.
func TestSelectsCheapestThatFits(t *testing.T) {
	offers := []core.Offer{
		offer("a", "A100", 1.29, 0.98, 80),
		offer("b", "A6000", 0.81, 0.91, 48),
		offer("c", "RTX 4090", 0.42, 0.97, 24),
		offer("d", "RTX 3060", 0.10, 0.99, 12), // cheapest, but too small
	}
	r := Select(offers, core.Criteria{}, needs(24), DefaultPolicy())
	if r.Selected == nil {
		t.Fatal("something should have been selected")
	}
	if r.Selected.Offer.OfferID != "c" {
		t.Fatalf("selected %s at $%.2f, want the $0.42 4090",
			r.Selected.Offer.OfferID, r.Selected.Offer.PriceHr)
	}
}

// Fit is a filter, not a scoring term. An 80 GB card serving a small model is
// wasteful, but if it is the cheapest thing that works it is the right answer
// — the earlier weighted design would have penalised it for "poor fit".
func TestOverProvisionedButCheapestStillWins(t *testing.T) {
	offers := []core.Offer{
		offer("big", "A100", 0.50, 0.95, 80), // gross over-provision, cheapest
		offer("snug", "RTX 4090", 0.90, 0.95, 24),
	}
	r := Select(offers, core.Criteria{}, needs(20), DefaultPolicy())
	if r.Selected.Offer.OfferID != "big" {
		t.Fatalf("selected %s; wasteful-but-cheapest must win once fit is answered",
			r.Selected.Offer.OfferID)
	}
}

// The trap the floors exist for. Live data showed a 4090 at $0.135 against a
// class spread reaching $8 — either a bargain on tired hardware or a host
// fishing for prompts, and the price alone does not say which.
func TestAnomalouslyCheapIsExcludedAndExplained(t *testing.T) {
	var offers []core.Offer
	for i := 0; i < 12; i++ {
		offers = append(offers, offer("n"+itoa(i), "RTX 4090", 0.90+float64(i)*0.02, 0.97, 24))
	}
	offers = append(offers, offer("bait", "RTX 4090", 0.135, 0.97, 24))

	r := Select(offers, core.Criteria{}, needs(20), DefaultPolicy())
	if r.Selected == nil {
		t.Fatal("the honest offers should still yield a selection")
	}
	if r.Selected.Offer.OfferID == "bait" {
		t.Fatal("an offer far below its class median must not be selected silently")
	}
	var bait *Candidate
	for i := range r.Candidates {
		if r.Candidates[i].Offer.OfferID == "bait" {
			bait = &r.Candidates[i]
		}
	}
	if bait == nil || bait.Reason != ReasonPriceOutlier {
		t.Fatalf("bait reason = %v, want price-outlier", bait)
	}
	// Excluded, not silently dropped: the operator can override knowingly.
	if !strings.Contains(bait.Detail, "below") || !strings.Contains(bait.Detail, "median") {
		t.Errorf("the exclusion must be explained, got %q", bait.Detail)
	}
}

// A class with few listings has no distribution, so the outlier test must not
// fire on it — otherwise a rare GPU with two offers becomes unbuyable.
func TestOutlierTestNeedsASample(t *testing.T) {
	offers := []core.Offer{
		offer("cheap", "H200", 0.20, 0.97, 141),
		offer("dear", "H200", 4.00, 0.97, 141),
	}
	r := Select(offers, core.Criteria{}, needs(80), DefaultPolicy())
	if r.Selected == nil || r.Selected.Offer.OfferID != "cheap" {
		t.Fatal("with only two listings there is no median to be an outlier against")
	}
}

func TestReliabilityFloorExcludes(t *testing.T) {
	offers := []core.Offer{
		offer("flaky", "RTX 4090", 0.30, 0.61, 24),
		offer("solid", "RTX 4090", 0.45, 0.97, 24),
	}
	r := Select(offers, core.Criteria{}, needs(20), DefaultPolicy())
	if r.Selected.Offer.OfferID != "solid" {
		t.Fatalf("selected %s; a host that vanishes mid-run costs more than it saved",
			r.Selected.Offer.OfferID)
	}
	ex := r.Excluded()
	if len(ex) == 0 || ex[0].Reason != ReasonReliability {
		t.Fatalf("the flaky host must be excluded with a reason, got %v", ex)
	}
	if !strings.Contains(ex[0].Detail, "0.61") || !strings.Contains(ex[0].Detail, "0.90") {
		t.Errorf("the detail should name both numbers, got %q", ex[0].Detail)
	}
}

// FR-SRCH-03: an operator asking "why not the cheap one" gets the reason, not
// a score.
func TestEveryExclusionCarriesAReason(t *testing.T) {
	offers := []core.Offer{
		offer("small", "RTX 3060", 0.10, 0.99, 12),
		offer("flaky", "RTX 4090", 0.20, 0.50, 24),
		offer("pricey", "A100", 9.00, 0.99, 80),
		offer("spot", "RTX 4090", 0.25, 0.99, 24),
		offer("good", "RTX 4090", 0.45, 0.99, 24),
	}
	offers[3].Interruptible = true
	c := core.Criteria{MaxPriceHr: 5.0}

	r := Select(offers, c, needs(20), DefaultPolicy())
	if r.Selected == nil || r.Selected.Offer.OfferID != "good" {
		t.Fatalf("selected %v", r.Selected)
	}
	want := map[string]Reason{
		"small":  ReasonVRAM,
		"flaky":  ReasonReliability,
		"pricey": ReasonMaxPrice,
		"spot":   ReasonInterruptible,
	}
	for _, cand := range r.Candidates {
		if w, ok := want[cand.Offer.OfferID]; ok {
			if cand.Reason != w {
				t.Errorf("%s reason = %q, want %q", cand.Offer.OfferID, cand.Reason, w)
			}
			if cand.Detail == "" {
				t.Errorf("%s excluded with no explanation", cand.Offer.OfferID)
			}
		}
	}
}

// Eligible-but-pricier offers are marked as such, rather than left looking
// like they failed a check they passed.
func TestCostlierOffersAreNotReportedAsFailures(t *testing.T) {
	offers := []core.Offer{
		offer("cheap", "RTX 4090", 0.40, 0.99, 24),
		offer("dear", "RTX 4090", 0.60, 0.99, 24),
	}
	r := Select(offers, core.Criteria{}, needs(20), DefaultPolicy())
	for _, c := range r.Candidates {
		if c.Offer.OfferID == "dear" {
			if c.Reason != ReasonCostlier {
				t.Errorf("reason = %q, want costlier-than-selection", c.Reason)
			}
			if !c.Eligible() {
				t.Error("a costlier offer was still eligible; it just lost on price")
			}
		}
	}
}

// Same market, same choice — otherwise a selection cannot be reproduced in a
// bug report.
func TestSelectionIsDeterministic(t *testing.T) {
	offers := []core.Offer{
		offer("x", "RTX 4090", 0.50, 0.95, 24),
		offer("y", "RTX 4090", 0.50, 0.99, 24), // same price, better reliability
		offer("z", "RTX 4090", 0.50, 0.99, 24), // same price and reliability
	}
	for i := 0; i < 20; i++ {
		r := Select(offers, core.Criteria{}, needs(20), DefaultPolicy())
		if r.Selected.Offer.OfferID != "y" {
			t.Fatalf("run %d selected %s; ties break by reliability then offer id",
				i, r.Selected.Offer.OfferID)
		}
	}
}

func TestNothingEligibleSelectsNothing(t *testing.T) {
	offers := []core.Offer{offer("small", "RTX 3060", 0.10, 0.99, 12)}
	r := Select(offers, core.Criteria{}, needs(80), DefaultPolicy())
	if r.Selected != nil {
		t.Fatal("nothing fits; selecting anyway would rent hardware that OOMs")
	}
	if len(r.Excluded()) != 1 {
		t.Fatal("the reason must still be available to explain the refusal")
	}
}

// The floors are overridable; the point is that they are not bypassed by
// accident.
func TestFloorsAreOverridable(t *testing.T) {
	offers := []core.Offer{
		offer("flaky", "RTX 4090", 0.20, 0.50, 24),
		offer("solid", "RTX 4090", 0.45, 0.99, 24),
	}
	loose := Policy{ReliabilityFloor: 0, OutlierFactor: 0, MinClassSample: 8}
	r := Select(offers, core.Criteria{}, needs(20), loose)
	if r.Selected.Offer.OfferID != "flaky" {
		t.Fatal("with the floors lowered the operator gets the cheap machine they asked for")
	}
}

// The failure a live run actually produced: the cheapest card whose VRAM held
// the model was a GTX 1060, which cannot serve with vLLM at any price because
// Pascal is below the compute capability vLLM's kernels require. VRAM fit
// answered the wrong question on its own.
func TestHardwareTooOldForTheRuntimeIsExcluded(t *testing.T) {
	pascal := offer("cheap", "GTX 1060", 0.017, 0.98, 6)
	pascal.ComputeCapability = 610
	volta := offer("works", "Tesla V100", 0.168, 0.98, 32)
	volta.ComputeCapability = 700

	// The FitFunc the orchestrator builds: capability first, then VRAM.
	fits := func(o core.Offer) (bool, string) {
		if o.ComputeCapability > 0 && o.ComputeCapability < 700 {
			return false, "compute capability too low for vLLM"
		}
		return o.VRAMTotalGB() >= 5, "too small"
	}
	r := Select([]core.Offer{pascal, volta}, core.Criteria{}, fits, DefaultPolicy())
	if r.Selected == nil || r.Selected.Offer.OfferID != "works" {
		t.Fatalf("selected %v; a card the engine cannot use is not a bargain", r.Selected)
	}
	var ex *Candidate
	for i := range r.Candidates {
		if r.Candidates[i].Offer.OfferID == "cheap" {
			ex = &r.Candidates[i]
		}
	}
	if ex == nil || ex.Reason != ReasonVRAM {
		t.Fatalf("the Pascal card must be excluded with a reason, got %v", ex)
	}
	if !strings.Contains(ex.Detail, "compute capability") {
		t.Errorf("the reason should name the constraint, got %q", ex.Detail)
	}
}

// A provider that reports no reliability must not have its whole catalogue
// rejected.
//
// This was a live trap for the second provider before one existed: RunPod
// lists GPU *types* with availability rather than named machines, so it has no
// per-host score to report. Every offer arrived at 0.00, the 0.90 floor
// excluded all of them, and the operator would have seen "no offer satisfies
// the criteria" — a market failure, for what was a modelling mistake.
func TestReliabilityFloorSkipsProvidersThatReportNone(t *testing.T) {
	offers := []core.Offer{
		{Provider: "catalogue", OfferID: "a", GPUModel: "RTX 4090", GPUCount: 1,
			VRAMPerGPUGB: 24, PriceHr: 0.40}, // no Reliability at all
		{Provider: "catalogue", OfferID: "b", GPUModel: "RTX 4090", GPUCount: 1,
			VRAMPerGPUGB: 24, PriceHr: 0.50},
	}
	res := Select(offers, core.Criteria{}, nil, DefaultPolicy())
	if res.Selected == nil {
		t.Fatal("a provider with no reliability score had every offer rejected")
	}
	for _, c := range res.Candidates {
		if c.Reason == ReasonReliability {
			t.Errorf("%s excluded on a score its provider does not publish", c.Offer.OfferID)
		}
	}
}

// And where a score *is* reported, the floor still bites. Relaxing it for the
// unreported case must not relax it for everyone.
func TestReliabilityFloorStillExcludesAReportedLowScore(t *testing.T) {
	offers := []core.Offer{
		{Provider: "market", OfferID: "bad", GPUModel: "RTX 4090", GPUCount: 1,
			VRAMPerGPUGB: 24, PriceHr: 0.10, Reliability: 0.40},
		{Provider: "market", OfferID: "good", GPUModel: "RTX 4090", GPUCount: 1,
			VRAMPerGPUGB: 24, PriceHr: 0.40, Reliability: 0.99},
	}
	res := Select(offers, core.Criteria{}, nil, DefaultPolicy())
	if res.Selected == nil || res.Selected.Offer.OfferID != "good" {
		t.Fatalf("selected %v; the cheap unreliable host should have been excluded", res.Selected)
	}
	var excluded bool
	for _, c := range res.Candidates {
		if c.Offer.OfferID == "bad" && c.Reason == ReasonReliability {
			excluded = true
		}
	}
	if !excluded {
		t.Error("a reported 0.40 was not excluded by the 0.90 floor")
	}
}

// The cheapest hourly rate is routinely the most expensive way to get a
// working endpoint, because the download is billed at that rate too. Live
// pair from one market: $0.216/hr on a 1347 Mbps link reaches ready in six
// minutes; $0.109/hr on a 68.7 Mbps link needs nearly two hours of billed
// downloading first — and is unusable for those two hours.
func TestSelectionWeighsTheBilledDownloadNotJustTheRate(t *testing.T) {
	const coldStart = 60 << 30 // a 27B model plus the runtime image

	fast := core.Offer{OfferID: "fast", Provider: "p", GPUModel: "RTX 3090",
		VRAMPerGPUGB: 24, GPUCount: 4, PriceHr: 0.216, Reliability: 0.99, NetDownMbps: 1347}
	cheap := core.Offer{OfferID: "cheap", Provider: "p", GPUModel: "RTX 3090",
		VRAMPerGPUGB: 24, GPUCount: 4, PriceHr: 0.109, Reliability: 0.99, NetDownMbps: 68.7}
	offers := []core.Offer{cheap, fast} // cheap first, so order cannot carry the result

	pol := func(hours float64) Policy {
		p := DefaultPolicy()
		p.ColdStartBytes = coldStart
		p.SessionHours = hours
		p.OutlierFactor = 0 // not what is under test
		return p
	}

	// A short session: the download dominates, so the fast link wins despite
	// costing twice as much per hour.
	got := Select(offers, core.Criteria{}, nil, pol(1))
	if got.Selected == nil || got.Selected.Offer.OfferID != "fast" {
		t.Errorf("1h session selected %v, want the fast link", selectedID(got))
	}

	// A long session: the download amortises away and the hourly rate is
	// what is left, so the cheap host is right after all.
	got = Select(offers, core.Criteria{}, nil, pol(24))
	if got.Selected == nil || got.Selected.Offer.OfferID != "cheap" {
		t.Errorf("24h session selected %v, want the cheap rate", selectedID(got))
	}

	// With nothing to download, the two are ranked as they always were.
	p := pol(1)
	p.ColdStartBytes = 0
	got = Select(offers, core.Criteria{}, nil, p)
	if got.Selected == nil || got.Selected.Offer.OfferID != "cheap" {
		t.Errorf("with no download, selected %v, want the cheap rate", selectedID(got))
	}
}

// A provider that publishes no link speed must neither win by default nor
// lose by default, so it is scored at the market's median.
func TestUnreportedLinkSpeedIsScoredAtTheMedian(t *testing.T) {
	known := []core.Offer{
		{OfferID: "a", NetDownMbps: 100}, {OfferID: "b", NetDownMbps: 500},
		{OfferID: "c", NetDownMbps: 900},
	}
	if got := medianNetMbps(known); got != 500 {
		t.Errorf("medianNetMbps = %v, want 500", got)
	}
	silent := core.Offer{OfferID: "silent", PriceHr: 1}
	h := coldStartHours(silent, 60<<30, 500)
	if h <= 0 {
		t.Fatal("an unreported link must still be given a cold start")
	}
	// Identical to an offer that reports the median explicitly.
	if h2 := coldStartHours(core.Offer{NetDownMbps: 500, PriceHr: 1}, 60<<30, 500); h != h2 {
		t.Errorf("silent %v vs explicit median %v", h, h2)
	}
	// And a market that reports nothing at all falls back to rate-only.
	if got := medianNetMbps([]core.Offer{{OfferID: "x"}}); got != 0 {
		t.Errorf("no speeds reported should give 0, got %v", got)
	}
}

func selectedID(r Result) string {
	if r.Selected == nil {
		return "<none>"
	}
	return r.Selected.Offer.OfferID
}
