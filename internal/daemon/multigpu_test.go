// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	pfake "go.sovrenix.com/larri/internal/provider/fake"
	"go.sovrenix.com/larri/internal/rank"
	rfake "go.sovrenix.com/larri/internal/runtime/fake"
	"go.sovrenix.com/larri/internal/sizing"
	"go.sovrenix.com/larri/internal/state"
)

// A large MoE: too big for any single card on the market, comfortable across
// two. 64 heads, so every power-of-two degree divides it.
var bigModel = sizing.Facts{
	Ref: "test/big", Params: 235, Layers: 94, AttentionHeads: 64, KVHeads: 4,
	HeadDim: 128, HiddenSize: 4096, MaxContextLen: 131072,
}

func multiGPUMarket() []core.Offer {
	return []core.Offer{
		{Provider: "fake", OfferID: "single", GPUModel: "H100 SXM", GPUCount: 1,
			VRAMPerGPUGB: 80, PriceHr: 2.69, Reliability: 0.99,
			MachineID: "m1", NetDownMbps: 1000},
		{Provider: "fake", OfferID: "pair", GPUModel: "RTX PRO 6000", GPUCount: 2,
			VRAMPerGPUGB: 96, PriceHr: 3.38, Reliability: 0.99,
			MachineID: "m2", NetDownMbps: 1000},
		{Provider: "fake", OfferID: "octet", GPUModel: "H100 SXM", GPUCount: 8,
			VRAMPerGPUGB: 80, PriceHr: 21.52, Reliability: 0.99,
			MachineID: "m3", NetDownMbps: 1000},
	}
}

func multiGPUOrch(t *testing.T, market []core.Offer, facts sizing.Facts) (*Orchestrator, *pfake.Provider) {
	t.Helper()
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := pfake.New("fake", market, pfake.Behaviour{})
	return &Orchestrator{
		Store: st, Provider: p, Runtime: rfake.New(rfake.Behaviour{}),
		Resolver: sizing.StaticResolver{"test/big": facts},
		Policy:   rank.DefaultPolicy(), Deadline: time.Minute,
	}, p
}

// bigModelReq names no disk, so the daemon sizes one to the model — which is
// what every surface does now. A fixed 50 here would be refused for any model
// this file is about, and these tests are about GPUs, not disks.
func bigModelReq() UpRequest {
	return UpRequest{
		Model: core.ModelSpec{Ref: "test/big", ServedName: "big",
			Quantization: "q4_K_M", ContextLen: 8192},
	}
}

// Issue #2: a model that exceeds the largest single card must be served on a
// host that has several, not reported unsatisfiable. The requirement here is
// ~122 GB — past an 80 GB H100, well inside two 96 GB cards.
func TestMultiGPUHostIsSelectedWhenNoSingleCardHoldsTheModel(t *testing.T) {
	o, _ := multiGPUOrch(t, multiGPUMarket(), bigModel)
	sv, err := o.Offers(context.Background(), bigModelReq())
	if err != nil {
		t.Fatalf("a model that fits two cards was refused: %v", err)
	}
	got := sv.Selection.Selected.Offer
	if got.OfferID != "pair" {
		t.Errorf("selected %s at $%.2f/hr; want the two-card host, which is the "+
			"cheapest thing that holds the model", got.Hardware(), got.PriceHr)
	}
	if sv.Plan.TensorParallelSize != 2 {
		t.Errorf("plan shards across %d cards, want 2 — the launch reads this, "+
			"and one card of a two-card host cannot hold the model",
			sv.Plan.TensorParallelSize)
	}
}

// The plan a dry run prints is the plan the launch uses. Leaving it at the
// single-card baseline until the host answers means the preview is wrong and,
// when the GPU probe fails, so is the launch.
func TestPlanIsSizedAgainstTheHostThatWasChosen(t *testing.T) {
	o, _ := multiGPUOrch(t, multiGPUMarket(), bigModel)
	sv, err := o.Offers(context.Background(), bigModelReq())
	if err != nil {
		t.Fatal(err)
	}
	chosen := sv.Selection.Selected.Offer
	avail := sizing.ShardVRAM(chosen.VRAMPerGPUGB, sv.Plan.TensorParallelSize)
	if !sv.Plan.FitsInVRAM {
		t.Error("the plan does not claim to fit the host it was sized against")
	}
	if sv.Plan.RequiredVRAMBytes > avail {
		t.Errorf("plan needs %s, the chosen host offers %s",
			sizing.HumanBytes(sv.Plan.RequiredVRAMBytes), sizing.HumanBytes(avail))
	}
	if sv.Plan.GPUMemUtilization <= 0 || sv.Plan.GPUMemUtilization > sizing.MaxGPUUtilisation {
		t.Errorf("memory fraction %v is the placeholder, not a figure derived from a card",
			sv.Plan.GPUMemUtilization)
	}
}

// A ceiling is a ceiling. An operator who does not want to pay for eight cards
// says so, and is not talked out of it by a ranking that finds them cheapest
// per gigabyte.
func TestMaxGPUsIsHonoured(t *testing.T) {
	o, _ := multiGPUOrch(t, multiGPUMarket(), bigModel)
	req := bigModelReq()
	req.Criteria = core.Criteria{MaxGPUCount: 1}
	_, err := o.Offers(context.Background(), req)
	if err == nil {
		t.Fatal("a one-card ceiling was ignored on a model no single card holds")
	}
	if !errs.Is(err, errs.ClassCriteriaUnsatisfiable) {
		t.Fatalf("class = %s, want criteria-unsatisfiable", errs.ClassOf(err))
	}
}

// Honouring the ceiling must not hide what it cost. The search never returned
// the hosts that would have worked, so "no offer has enough VRAM" is true of
// the question asked and misleading about the market. Issue #2 asked for the
// multi-GPU alternative to be named before declaring the model unsatisfiable.
func TestACeilingThatRuledOutTheFitNamesIt(t *testing.T) {
	o, p := multiGPUOrch(t, multiGPUMarket(), bigModel)
	req := bigModelReq()
	req.Criteria = core.Criteria{MaxGPUCount: 1}
	_, err := o.Offers(context.Background(), req)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"2× RTX PRO 6000 192GB", "raise --max-gpus to 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name the multi-GPU fit (%q): %v", want, err)
		}
	}
	if p.Count() != 0 {
		t.Fatal("naming an alternative must not rent it")
	}
}

// With no ceiling nothing was ruled out on card count, so there is nothing to
// look for: a second search would cost a round trip and say nothing new.
func TestNoCeilingMeansNoSecondSearch(t *testing.T) {
	market := []core.Offer{
		{Provider: "fake", OfferID: "small", GPUModel: "RTX 4090", GPUCount: 1,
			VRAMPerGPUGB: 24, PriceHr: 0.40, Reliability: 0.99, MachineID: "m1"},
	}
	o, p := multiGPUOrch(t, market, bigModel)
	if _, err := o.Offers(context.Background(), bigModelReq()); err == nil {
		t.Fatal("expected a refusal")
	}
	searches := 0
	for _, c := range p.Calls {
		if c == "Search" {
			searches++
		}
	}
	if searches != 1 {
		t.Errorf("searched %d times with no ceiling to lift", searches)
	}
}

// A multi-GPU placement is explained card by card: an aggregate figure hides
// whether one card is at its ceiling, and says nothing about what the extra
// cards buy. For a layer-splitting engine that is memory, not speed.
func TestShardedPlacementIsExplainedPerCard(t *testing.T) {
	o, _ := multiGPUOrch(t, multiGPUMarket(), bigModel)
	events := make(chan Event, 256)
	o.Events = events
	if _, err := o.Offers(context.Background(), bigModelReq()); err != nil {
		t.Fatal(err)
	}
	close(events)
	var lines []string
	for e := range events {
		lines = append(lines, e.Message)
	}
	all := strings.Join(lines, "\n")
	for _, want := range []string{"sharding across all 2 cards", "per card: about", "usable",
		"take turns on each token"} {
		if !strings.Contains(all, want) {
			t.Errorf("placement not explained (%q missing):\n%s", want, all)
		}
	}
}

// A tensor-parallel engine runs every card on every token, so the layer-split
// caveat would be false there and must not be said.
func TestTensorParallelPlacementMakesNoLayerSplitClaim(t *testing.T) {
	o, _ := multiGPUOrch(t, multiGPUMarket(), bigModel)
	o.Runtime = rfake.New(rfake.Behaviour{TensorParallel: true})
	events := make(chan Event, 256)
	o.Events = events
	if _, err := o.Offers(context.Background(), bigModelReq()); err != nil {
		t.Fatal(err)
	}
	close(events)
	for e := range events {
		if strings.Contains(e.Message, "take turns") {
			t.Errorf("layer-split caveat said of a tensor-parallel engine: %s", e.Message)
		}
	}
}

// vLLM refuses a tensor-parallel degree that does not divide the model's
// attention heads, and it refuses it at engine init — on a machine that is
// already billing. So the cards it cannot reach must not be counted as VRAM
// during selection.
func TestOddCardCountIsSizedOnTheCardsTheEngineCanReach(t *testing.T) {
	// 40 heads: three cards shard across two, six across four.
	facts := bigModel
	facts.AttentionHeads = 40
	facts.KVHeads = 8
	facts.Params = 70

	market := []core.Offer{
		// 6× 24GB advertises 144 GB, but 40 heads only shard four ways, so
		// the engine reaches 96 GB — not enough at fp16.
		{Provider: "fake", OfferID: "six", GPUModel: "RTX 3090", GPUCount: 6,
			VRAMPerGPUGB: 24, PriceHr: 1.20, Reliability: 0.99,
			MachineID: "m1", NetDownMbps: 1000},
	}
	o, p := multiGPUOrch(t, market, facts)
	o.Runtime = rfake.New(rfake.Behaviour{TensorParallel: true})

	req := bigModelReq()
	req.Model.Quantization = "fp16"
	_, err := o.Offers(context.Background(), req)
	if err == nil {
		t.Fatal("a host whose cards the engine cannot all reach was accepted")
	}
	if !strings.Contains(err.Error(), "shards across 4 of 6 cards") {
		t.Errorf("the refusal does not say why six cards were not six cards: %v", err)
	}
	// The two figures on that line have to reconcile: 144GB advertised and
	// 67.3 GB short of 158.5 GB only add up once the reachable total is said.
	if !strings.Contains(err.Error(), "91.2 GB usable") {
		t.Errorf("the refusal names no reachable VRAM, so its arithmetic reads as a bug: %v", err)
	}
	if p.Count() != 0 {
		t.Fatal("nothing may be rented when the engine cannot use the hardware")
	}
}

// A runtime that has measured its own weights must be believed over the
// estimate, all the way into selection. The estimate is a parameter count
// times a table of average bits per weight, and unsloth's UD-IQ1_M is 3.31
// bits where its name says 1.75 — a 180B model sized from the table lands at
// 39 GB against a real 69 GB, which selects one card for a four-card model.
func TestMeasuredWeightsReachSelection(t *testing.T) {
	market := []core.Offer{
		{Provider: "fake", OfferID: "one", GPUModel: "L40S", GPUCount: 1,
			VRAMPerGPUGB: 48, PriceHr: 0.79, Reliability: 0.99,
			MachineID: "m1", NetDownMbps: 1000},
		{Provider: "fake", OfferID: "four", GPUModel: "L40S", GPUCount: 4,
			VRAMPerGPUGB: 48, PriceHr: 3.16, Reliability: 0.99,
			MachineID: "m2", NetDownMbps: 1000},
	}
	// 180B at the table's 1.75 bits for iq1_m is ~39 GB of weights, which
	// fits the single 48 GB card. Measured, it is 69 GB and does not.
	facts := sizing.Facts{
		Ref: "test/big", Params: 180, Layers: 62, AttentionHeads: 64, KVHeads: 4,
		HeadDim: 128, HiddenSize: 2048, MaxContextLen: 131072,
	}
	req := bigModelReq()
	req.Model.Quantization = "iq1_m"

	estimated, _ := multiGPUOrch(t, market, facts)
	sv, err := estimated.Offers(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if sv.Selection.Selected.Offer.OfferID != "one" {
		t.Fatalf("the estimate already rejects the single card; the test proves nothing")
	}

	measured, _ := multiGPUOrch(t, market, facts)
	measured.Runtime = rfake.New(rfake.Behaviour{WeightBytes: 69 << 30})
	sv, err = measured.Offers(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := sv.Selection.Selected.Offer; got.OfferID != "four" {
		t.Errorf("selected %s on measured 69 GB weights; a 48 GB card cannot hold them",
			got.Hardware())
	}
	if sv.Plan.WeightsBytes != 69<<30 {
		t.Errorf("plan carries %s of weights, not the measured 69.0 GB",
			sizing.HumanBytes(sv.Plan.WeightsBytes))
	}
}

// Nothing used to ask whether the disk could hold the weights: every rental
// got 60 GB, and a 111 GB model filled it minutes into a download on a host
// already billing. A disk nobody named is now sized to the model.
func TestDiskIsSizedToTheWeightsWhenNobodyNamedOne(t *testing.T) {
	o, p := multiGPUOrch(t, multiGPUMarket(), bigModel)
	o.Runtime = rfake.New(rfake.Behaviour{WeightBytes: 111_000_000_000})
	sv, err := o.Offers(context.Background(), bigModelReq())
	if err != nil {
		t.Fatal(err)
	}
	if sv.DiskGB < 111 {
		t.Errorf("disk %d GB for 111 GB of weights", sv.DiskGB)
	}
	if want := DiskNeedGB(sv.Plan); sv.DiskGB != want {
		t.Errorf("disk %d GB, want the model's need of %d", sv.DiskGB, want)
	}
	// The search has to filter on the same figure the create will ask for,
	// or it ranks hosts that cannot be given that disk.
	if got := p.LastSearch().DiskGB; got != sv.DiskGB {
		t.Errorf("searched for %d GB of disk, will rent %d", got, sv.DiskGB)
	}
}

// A figure the operator named spends money, so it is theirs to change. Too
// small to hold the weights is refused before the search — never raised
// behind their back, and never discovered when the download fills the disk.
func TestANamedDiskTooSmallForTheWeightsIsRefused(t *testing.T) {
	o, p := multiGPUOrch(t, multiGPUMarket(), bigModel)
	o.Runtime = rfake.New(rfake.Behaviour{WeightBytes: 111_000_000_000})
	req := bigModelReq()
	req.DiskGB = 60
	_, err := o.Offers(context.Background(), req)
	if err == nil {
		t.Fatal("a 60 GB disk was accepted for 111 GB of weights")
	}
	if !errs.Is(err, errs.ClassCriteriaUnsatisfiable) {
		t.Errorf("class = %s, want criteria-unsatisfiable", errs.ClassOf(err))
	}
	if !strings.Contains(err.Error(), "--disk") {
		t.Errorf("the refusal names no remedy: %v", err)
	}
	if p.Count() != 0 {
		t.Fatal("nothing may be rented when the disk cannot hold the model")
	}
}

// A small model keeps the disk every model used to get, so nothing that
// worked before this change rents a different disk now.
func TestSmallModelsKeepTheOldDefault(t *testing.T) {
	o, _, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{WeightBytes: 5_000_000_000})
	req := upReq()
	req.DiskGB = 0 // nobody named one
	sv, err := o.Offers(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if sv.DiskGB != DefaultDiskGB {
		t.Errorf("disk %d GB for a 5 GB model, want the %d GB floor", sv.DiskGB, DefaultDiskGB)
	}
}

// The floor is for a disk nobody named. A named one that holds the weights is
// the operator's call even when it is below the floor: 50 GB for an 8B model
// is plenty, and refusing it would be a regression dressed as a safety check.
func TestANamedDiskBelowTheFloorThatFitsIsHonoured(t *testing.T) {
	o, _, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{WeightBytes: 5_000_000_000})
	req := upReq()
	req.DiskGB = 30
	sv, err := o.Offers(context.Background(), req)
	if err != nil {
		t.Fatalf("a 30 GB disk for a 5 GB model was refused: %v", err)
	}
	if sv.DiskGB != 30 {
		t.Errorf("disk %d GB, want the 30 the operator named", sv.DiskGB)
	}
}

// When the model fits no card and the named disk is also too small, the VRAM
// shortfall is the answer that matters: the disk is one flag, and reporting it
// first sends the operator to fix it only to meet the larger problem next run.
func TestVRAMShortfallIsReportedBeforeADiskShortfall(t *testing.T) {
	o, _ := multiGPUOrch(t, multiGPUMarket(), bigModel)
	req := bigModelReq()
	req.DiskGB = 50
	req.Criteria.MaxGPUCount = 1 // no single card holds it
	_, err := o.Offers(context.Background(), req)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "needs ~") {
		t.Errorf("the VRAM shortfall was not reported first: %v", err)
	}
}
