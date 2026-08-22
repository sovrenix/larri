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

var facts = sizing.Facts{
	Ref: "test/model", Params: 8.03, Layers: 32, KVHeads: 8,
	HeadDim: 128, HiddenSize: 4096, MaxContextLen: 131072,
}

func offers() []core.Offer {
	var out []core.Offer
	for i := 0; i < 10; i++ {
		out = append(out, core.Offer{
			Provider: "fake", OfferID: string(rune('a' + i)), GPUModel: "RTX 4090",
			GPUCount: 1, VRAMPerGPUGB: 24, PriceHr: 0.40 + float64(i)*0.05,
			Reliability: 0.97,
		})
	}
	return out
}

func newOrch(t *testing.T, b pfake.Behaviour, rb rfake.Behaviour) (*Orchestrator, *pfake.Provider, *state.Store) {
	t.Helper()
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := pfake.New("fake", offers(), b)
	o := &Orchestrator{
		Store: st, Provider: p, Runtime: rfake.New(rb),
		Resolver: sizing.StaticResolver{"test/model": facts},
		Policy:   rank.DefaultPolicy(), Deadline: time.Minute,
	}
	return o, p, st
}

func upReq() UpRequest {
	return UpRequest{
		Criteria: core.Criteria{},
		Model: core.ModelSpec{
			Ref: "test/model", ServedName: "test", Quantization: "q4_K_M", ContextLen: 8192,
		},
		DiskGB: 50,
	}
}

func TestUpSelectsCheapestAndCreates(t *testing.T) {
	o, p, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	rig, err := o.Up(context.Background(), upReq())
	if err != nil {
		t.Fatal(err)
	}
	if rig.Offer.PriceHr != 0.40 {
		t.Errorf("selected $%.2f/hr, want the cheapest 0.40", rig.Offer.PriceHr)
	}
	if rig.Instance == nil {
		t.Fatal("an instance should exist")
	}
	if p.Count() != 1 {
		t.Errorf("provider holds %d instances, want 1", p.Count())
	}
}

// AC-2.1 at the orchestrator level: the intent precedes the spend, so a create
// that never answers still leaves the rig findable.
func TestIntentIsJournalledBeforeTheCreateCall(t *testing.T) {
	o, p, st := newOrch(t, pfake.Behaviour{CreateTimesOutButSucceeds: true}, rfake.Behaviour{})

	rig, err := o.Up(context.Background(), upReq())
	if err == nil {
		t.Fatal("a create with an unknown outcome must surface as an error")
	}
	if !errs.Is(err, errs.ClassProviderUnknownOutcome) {
		t.Fatalf("class = %s, want provider-unknown-outcome", errs.ClassOf(err))
	}
	// The instance exists despite the error.
	if p.Count() != 1 {
		t.Fatalf("provider holds %d instances; the create landed", p.Count())
	}
	// And the journal knew about it before the call was made.
	entries, err := st.Entries()
	if err != nil {
		t.Fatal(err)
	}
	mine := state.EntriesFor(entries, rig.ID)
	if len(mine) == 0 {
		t.Fatal("nothing was journalled; the instance would be unattributable")
	}
	var sawCreating bool
	for _, e := range mine {
		if e.To == core.StateCreating {
			sawCreating = true
			if e.Provider == "" || e.Offer == "" {
				t.Error("the intent must name the provider and offer to search by")
			}
		}
	}
	if !sawCreating {
		t.Error("a CREATING intent must precede the spend")
	}
	// The orphan is attributable by label.
	live, _ := p.List(context.Background())
	if id, ours := live[0].RigID(); !ours || id != rig.ID {
		t.Errorf("instance label = %q ours=%v, want rig %s", id, ours, rig.ID)
	}
}

// NFR-11: a model that fits nothing is rejected before anything is created.
func TestUnfittableModelIsRejectedPreSpend(t *testing.T) {
	big := facts
	big.Params = 400 // no 24 GB card holds this
	o, p, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	o.Resolver = sizing.StaticResolver{"test/model": big}

	_, err := o.Up(context.Background(), upReq())
	if err == nil {
		t.Fatal("expected a pre-spend rejection")
	}
	if !errs.Is(err, errs.ClassCriteriaUnsatisfiable) {
		t.Fatalf("class = %s, want criteria-unsatisfiable", errs.ClassOf(err))
	}
	if p.Count() != 0 {
		t.Fatal("nothing may be rented when nothing fits")
	}
	if !strings.Contains(err.Error(), "needs ~") {
		t.Errorf("the rejection should state the VRAM required: %v", err)
	}
}

func TestConfirmationCanCancelBeforeSpending(t *testing.T) {
	o, p, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	req := upReq()
	req.Confirm = func(core.Offer, core.SizingPlan) bool { return false }

	if _, err := o.Up(context.Background(), req); err == nil {
		t.Fatal("declining confirmation must abort")
	}
	if p.Count() != 0 {
		t.Fatal("declining must not rent anything")
	}
}

// Teardown proves absence rather than trusting the delete call.
func TestDownConfirmsAbsence(t *testing.T) {
	o, p, st := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	rig, err := o.Up(context.Background(), upReq())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Down(context.Background(), rig, nil); err != nil {
		t.Fatal(err)
	}
	if p.Count() != 0 {
		t.Fatal("the instance should be gone")
	}
	if rig.State != core.StateDestroyed {
		t.Errorf("state = %s, want DESTROYED", rig.State)
	}
	if rig.End == nil {
		t.Fatal("a terminated rig must record why")
	}
	if rig.End.Actor != core.ActorOperator {
		t.Errorf("actor = %s, want operator", rig.End.Actor)
	}
	// Cost is replayed from the journal, so it survives a restart.
	entries, _ := st.Entries()
	if c := state.CostFor(entries, rig.ID, time.Now()); c.TotalUSD <= 0 {
		t.Error("a rig that ran should have accrued something")
	}
}

// R-13: a destroy that only stops leaves a storage-billing container, and the
// absence check is what catches it.
func TestDestroyThatOnlyStopsIsNotConfirmed(t *testing.T) {
	o, p, _ := newOrch(t, pfake.Behaviour{DestroyOnlyStops: true}, rfake.Behaviour{})
	o.Deadline = 30 * time.Second
	rig, err := o.Up(context.Background(), upReq())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	err = o.Down(ctx, rig, nil)
	if err == nil {
		t.Fatal("a stopped-but-present instance must not be reported destroyed")
	}
	if rig.State == core.StateDestroyed {
		t.Error("state must not reach DESTROYED without proof of absence")
	}
	if p.Count() != 1 {
		t.Error("precondition: the container still exists and still bills storage")
	}
}

// The standing disclosure names the specific machine, because "rented
// hardware" is a category and a named instance in a named place is not.
func TestPrivacyNoticeNamesTheHost(t *testing.T) {
	o, _, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	rig, err := o.Up(context.Background(), upReq())
	if err != nil {
		t.Fatal(err)
	}
	n := PrivacyNotice(rig)
	if !strings.Contains(n, rig.Instance.InstanceID) {
		t.Errorf("notice should name the instance: %s", n)
	}
	if !strings.Contains(strings.ToLower(n), "read your prompts") {
		t.Errorf("notice should state what the host can see: %s", n)
	}
}
