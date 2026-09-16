// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	pfake "go.sovrenix.com/larri/internal/provider/fake"
	"go.sovrenix.com/larri/internal/rank"
	rfake "go.sovrenix.com/larri/internal/runtime/fake"
	"go.sovrenix.com/larri/internal/sizing"
	"go.sovrenix.com/larri/internal/state"
	"time"
)

// Issue #3's market: the only card that holds the model is at low stock, and
// everything in stock is too small.
func lowStockMarket() []core.Offer {
	return []core.Offer{
		{Provider: "fake", OfferID: "small", GPUModel: "RTX 4090", GPUCount: 1,
			VRAMPerGPUGB: 24, PriceHr: 0.40, Reliability: 0.99, MachineID: "m1", NetDownMbps: 1000},
		{Provider: "fake", OfferID: "b200", GPUModel: "B200", GPUCount: 4,
			VRAMPerGPUGB: 180, PriceHr: 27.16, Reliability: 0.99, MachineID: "m2", NetDownMbps: 1000,
			LowStock: true},
	}
}

// Without the flag, low stock is not rented — but "no offer has enough VRAM"
// was true only of the offers weighed, and said nothing of the card that
// would have fitted. The refusal names it, and what to ask for.
func TestTheRefusalNamesALowStockOfferThatWouldFit(t *testing.T) {
	o, p := multiGPUOrch(t, lowStockMarket(), bigModel)
	_, err := o.Offers(context.Background(), bigModelReq())
	if err == nil {
		t.Fatal("rented at low stock without being allowed to")
	}
	for _, want := range []string{"At low stock: 4× B200 720GB", "--allow-low-stock"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name the low-stock fit (%q): %v", want, err)
		}
	}
	if p.Count() != 0 {
		t.Fatal("naming an alternative must not rent it")
	}
}

// Allowed, the low-stock offer is ranked like any other, and choosing it says
// so before anything is spent.
func TestAllowingLowStockSelectsItAndSaysSo(t *testing.T) {
	o, _ := multiGPUOrch(t, lowStockMarket(), bigModel)
	events := make(chan Event, 256)
	o.Events = events
	warnedc := make(chan bool, 1)
	go func() {
		var warned bool
		for e := range events {
			e.Show() // acknowledges a sync, as a surface does
			if e.Phase == "select" && e.Warning && strings.Contains(e.Message, "low stock") {
				warned = true
			}
		}
		warnedc <- warned
	}()
	req := bigModelReq()
	req.Criteria.AllowLowStock = true
	if _, err := o.Up(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	close(events)
	warned := <-warnedc
	if !warned {
		t.Error("a low-stock offer was chosen without saying so")
	}
}

// A provider with no notion of stock is not searched a second time to find
// out it has none.
func TestNoLowStockMeansNoSecondSearch(t *testing.T) {
	market := lowStockMarket()[:1]
	o, p := multiGPUOrch(t, market, bigModel)
	_, _ = o.Offers(context.Background(), bigModelReq())
	searches := 0
	for _, c := range p.Calls {
		if c == "Search" {
			searches++
		}
	}
	if searches != 1 {
		t.Errorf("searched %d times; nothing was withheld for stock", searches)
	}
}

// Allowing low stock surfaced a trap: the cheapest card RunPod lists with
// enough memory for a large model is AMD's MI300X, at low stock, and every
// image LARRI runs is CUDA. Cheapest or not, it is not a fit.
func TestAnAMDCardIsNotAFitForACUDAImage(t *testing.T) {
	market := []core.Offer{
		{Provider: "fake", OfferID: "amd", GPUModel: "MI300X", GPUCount: 1, VRAMPerGPUGB: 192,
			PriceHr: 2.39, Reliability: 0.99, MachineID: "m1", NetDownMbps: 1000, GPUVendor: "amd"},
		{Provider: "fake", OfferID: "b200", GPUModel: "B200", GPUCount: 2, VRAMPerGPUGB: 180,
			PriceHr: 13.58, Reliability: 0.99, MachineID: "m2", NetDownMbps: 1000, GPUVendor: "nvidia"},
	}
	o, _ := multiGPUOrch(t, market, bigModel)
	o.Runtime = rfake.New(rfake.Behaviour{Vendor: "nvidia"})
	sv, err := o.Offers(context.Background(), bigModelReq())
	if err != nil {
		t.Fatal(err)
	}
	if got := sv.Selection.Selected.Offer.OfferID; got != "b200" {
		t.Errorf("selected %s; an AMD card cannot run a CUDA image at any price", got)
	}
	for _, c := range sv.Selection.Candidates {
		if c.Offer.OfferID == "amd" && !strings.Contains(c.Detail, "amd gpu") {
			t.Errorf("the AMD card was set aside without saying why: %q", c.Detail)
		}
	}
}

// Every refusal is filed under the question that failed. They were all filed
// as insufficient VRAM, so a live survey reported "38 offers:
// insufficient-vram" with examples about a network floor, and an AMD card with
// 192 GB read as too small for the model.
func TestEachExcludedOfferNamesTheQuestionItFailed(t *testing.T) {
	market := []core.Offer{
		{Provider: "fake", OfferID: "amd", GPUModel: "MI300X", GPUCount: 1, VRAMPerGPUGB: 192,
			PriceHr: 2.39, Reliability: 0.99, MachineID: "m1", NetDownMbps: 1000, GPUVendor: "amd"},
		{Provider: "fake", OfferID: "slow", GPUModel: "B200", GPUCount: 2, VRAMPerGPUGB: 180,
			PriceHr: 9.10, Reliability: 0.99, MachineID: "m2", NetDownMbps: 60, GPUVendor: "nvidia"},
		{Provider: "fake", OfferID: "small", GPUModel: "RTX 4090", GPUCount: 1, VRAMPerGPUGB: 24,
			PriceHr: 0.40, Reliability: 0.99, MachineID: "m3", NetDownMbps: 1000, GPUVendor: "nvidia"},
		{Provider: "fake", OfferID: "b200", GPUModel: "B200", GPUCount: 2, VRAMPerGPUGB: 180,
			PriceHr: 13.58, Reliability: 0.99, MachineID: "m4", NetDownMbps: 1000, GPUVendor: "nvidia"},
	}
	o, _ := multiGPUOrch(t, market, bigModel)
	o.Runtime = rfake.New(rfake.Behaviour{Vendor: "nvidia"})
	req := bigModelReq()
	req.Criteria.MinNetMbps = 500
	sv, err := o.Offers(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]rank.Reason{
		"amd": rank.ReasonEngine, "slow": rank.ReasonNetwork, "small": rank.ReasonVRAM,
	}
	for _, c := range sv.Selection.Candidates {
		if w, ok := want[c.Offer.OfferID]; ok && c.Reason != w {
			t.Errorf("%s excluded as %q (%s), want %q", c.Offer.OfferID, c.Reason, c.Detail, w)
		}
	}
	if got := sv.Selection.Selected.Offer.OfferID; got != "b200" {
		t.Errorf("selected %s", got)
	}
}

// RunPod refuses the criteria itself when every type it lists is filtered
// out, and that refusal was returned before any advice: `--gpu B200` answered
// "nothing rentable" while B200s were listed at low stock.
func TestAProviderRefusalStillNamesWhatWouldFit(t *testing.T) {
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := pfake.New("fake", lowStockMarket()[1:], pfake.Behaviour{RefuseEmptySearch: true})
	o := &Orchestrator{
		Store: st, Provider: p, Runtime: rfake.New(rfake.Behaviour{}),
		Resolver: sizing.StaticResolver{"test/big": bigModel},
		Policy:   rank.DefaultPolicy(), Deadline: time.Minute,
	}
	_, err = o.Offers(context.Background(), bigModelReq())
	if err == nil {
		t.Fatal("rented at low stock without being allowed to")
	}
	for _, want := range []string{"nothing rentable", "At low stock: 4× B200", "--allow-low-stock"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	if !errs.Is(err, errs.ClassCriteriaUnsatisfiable) {
		t.Errorf("class = %s; still an unsatisfiable request", errs.ClassOf(err))
	}
}

// The guarantee the flag rests on: a low-stock create the provider refuses
// creates nothing, costs nothing, and moves on — to a different listing, not
// the refused one again — leaving nothing billing.
func TestARefusedLowStockCreateFallsBackAndLeavesNothing(t *testing.T) {
	market := []core.Offer{
		{Provider: "fake", OfferID: "low", GPUModel: "A40", GPUCount: 1, VRAMPerGPUGB: 48,
			PriceHr: 0.49, NetDownMbps: 1000, LowStock: true},
		{Provider: "fake", OfferID: "instock", GPUModel: "L40S", GPUCount: 1, VRAMPerGPUGB: 48,
			PriceHr: 1.09, NetDownMbps: 1000},
	}
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := pfake.New("fake", market, pfake.Behaviour{RefuseLowStock: true})
	o := &Orchestrator{
		Store: st, Provider: p, Runtime: rfake.New(rfake.Behaviour{}),
		Resolver: sizing.StaticResolver{"test/m": sizing.Facts{
			Ref: "test/m", Params: 8, Layers: 32, AttentionHeads: 32, KVHeads: 8,
			HeadDim: 128, HiddenSize: 4096, MaxContextLen: 32768}},
		Policy: rank.DefaultPolicy(), Deadline: 2 * time.Second, MaxHostAttempts: 2,
	}
	// The fake's hosts never serve, so the bring-up fails overall; what is
	// under test is what the refusal did on the way.
	_, _ = o.UpAndServe(context.Background(), UpRequest{
		Criteria: core.Criteria{AllowLowStock: true},
		Model:    core.ModelSpec{Ref: "test/m", ServedName: "m", Quantization: "q4_K_M", ContextLen: 4096},
	})

	rigs, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := st.Entries()
	byOffer := map[string]*core.Rig{}
	for _, r := range rigs {
		byOffer[r.Offer.OfferID] = r
	}
	refused, next := byOffer["low"], byOffer["instock"]
	if refused == nil || next == nil {
		t.Fatalf("attempted %v; want the low-stock listing, then the in-stock one", byOffer)
	}
	if refused.State != core.StateDestroyed || refused.End == nil ||
		refused.End.Evidence[core.EvidenceNothingCreated] == "" {
		t.Errorf("refused rig: state %s, end %+v; want closed as never created", refused.State, refused.End)
	}
	if c := state.CostForRig(entries, refused, time.Now().UTC().Add(time.Hour)); c.TotalUSD != 0 {
		t.Errorf("the refused create accrued $%.4f", c.TotalUSD)
	}
	if p.Count() != 0 {
		t.Errorf("%d instance(s) left billing", p.Count())
	}
}
