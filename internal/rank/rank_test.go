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
