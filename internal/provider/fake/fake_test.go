// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package fake

import (
	"context"
	"testing"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/provider"
)

func offers() []core.Offer {
	return []core.Offer{
		{Provider: "fake", OfferID: "a", GPUModel: "A100", GPUCount: 1, VRAMPerGPUGB: 80,
			PriceHr: 1.29, Reliability: 0.98, Certified: true},
		{Provider: "fake", OfferID: "b", GPUModel: "A100", GPUCount: 1, VRAMPerGPUGB: 80,
			PriceHr: 0.64, Reliability: 0.71, Interruptible: true},
	}
}

func mk(b Behaviour) (*Provider, context.Context) {
	return New("fake", offers(), b), context.Background()
}

// Q-04: interruptible is opt-in, so an unset criterion must exclude the
// cheaper interruptible offer rather than prefer it.
func TestSearchExcludesInterruptibleByDefault(t *testing.T) {
	p, ctx := mk(Behaviour{})
	got, err := p.Search(ctx, core.Criteria{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].OfferID != "a" {
		t.Fatalf("default search returned %d offers (%v); interruptible must be opt-in", len(got), got)
	}
	got, err = p.Search(ctx, core.Criteria{Interruptible: core.Allow})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Allow should return both offers, got %d", len(got))
	}
}

// R-07: a create that times out but actually succeeded must not be
// blind-retried. The fake proves the hazard is real — the instance exists
// despite the error.
func TestCreateTimeoutLeavesAnInstanceBehind(t *testing.T) {
	p, ctx := mk(Behaviour{CreateTimesOutButSucceeds: true})
	inst, err := p.Create(ctx, offers()[0], provider.CreateSpec{Label: "01J9Z"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if inst != nil {
		t.Fatal("no instance should be returned when the outcome is unknown")
	}
	if !errs.Is(err, errs.ClassProviderUnknownOutcome) {
		t.Fatalf("class = %s, want provider-unknown-outcome", errs.ClassOf(err))
	}
	if errs.Retryable(err) {
		t.Fatal("unknown-outcome must never be retryable: the retry is how one instance becomes two")
	}
	if p.Count() != 1 {
		t.Fatalf("the instance exists despite the error; Count() = %d, want 1", p.Count())
	}
	// Recovery is by label, not by retry.
	live, err := p.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("List should surface the orphan, got %d", len(live))
	}
	if id, ok := live[0].RigID(); !ok || id != "01J9Z" {
		t.Fatalf("the instance must be attributable by label, got %q,%v", id, ok)
	}
}

// R-13: a destroy that only stops still bills. A caller trusting the nil error
// would journal DESTROYED while storage accrued forever.
func TestDestroyThatOnlyStopsIsNotProofOfDestruction(t *testing.T) {
	p, ctx := mk(Behaviour{DestroyOnlyStops: true})
	inst, err := p.Create(ctx, offers()[0], provider.CreateSpec{Label: "01J9Z"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Destroy(ctx, inst.InstanceID); err != nil {
		t.Fatalf("the API reports success: %v", err)
	}
	got, err := p.Get(ctx, inst.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("instance should still exist: stopped is not gone")
	}
	if got.Running {
		t.Fatal("instance should be stopped")
	}
	if p.Count() != 1 {
		t.Fatal("a stopped container still exists and still bills storage")
	}
}

// The subtler half of R-13: if List omits non-running instances, the absence
// check that teardown relies on returns a false negative.
func TestListOmittingStoppedHidesABillingInstance(t *testing.T) {
	p, ctx := mk(Behaviour{DestroyOnlyStops: true, ListOmitsStopped: true})
	inst, err := p.Create(ctx, offers()[0], provider.CreateSpec{Label: "01J9Z"})
	if err != nil {
		t.Fatal(err)
	}
	_ = p.Destroy(ctx, inst.InstanceID)

	live, err := p.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatal("precondition: this fake is simulating a List that hides stopped instances")
	}
	// The instance is still there. An adapter with this bug would let
	// teardown declare success.
	if p.Count() != 1 {
		t.Fatal("the container still exists and still bills")
	}
	if got, _ := p.Get(ctx, inst.InstanceID); got == nil {
		t.Fatal("Get must still see what List hid — this is why the contract requires both")
	}
}

// R-14: a stopped interruptible can resume by itself. With a replacement
// already provisioned, that is two billing instances.
func TestStopThenResumeProducesTwoBillingInstances(t *testing.T) {
	p, ctx := mk(Behaviour{})
	first, _ := p.Create(ctx, offers()[1], provider.CreateSpec{Label: "01J9Z"})
	p.Stop(first.InstanceID) // outbid

	replacement, _ := p.Create(ctx, offers()[0], provider.CreateSpec{Label: "01JA2"})
	p.Resume(first.InstanceID) // the bid clears again

	live, err := p.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	running := 0
	for _, i := range live {
		if i.Running {
			running++
		}
	}
	if running != 2 {
		t.Fatalf("running instances = %d, want 2 — the resume hazard must be reproducible", running)
	}
	if first.InstanceID == replacement.InstanceID {
		t.Fatal("replacement should be a distinct instance")
	}
}

// FR-SUP-11: a failed query resolves nothing. It must not look like absence.
func TestUnreachableIsNotAbsence(t *testing.T) {
	p, ctx := mk(Behaviour{})
	inst, _ := p.Create(ctx, offers()[0], provider.CreateSpec{Label: "01J9Z"})
	p.SetUnreachable(true)

	got, err := p.Get(ctx, inst.InstanceID)
	if err == nil {
		t.Fatal("an unreachable provider must return an error, not a nil instance")
	}
	if got != nil {
		t.Fatal("no instance should be returned")
	}
	// The distinction that matters: nil,nil means absent; nil,err means unknown.
	p.SetUnreachable(false)
	got, err = p.Get(ctx, inst.InstanceID)
	if err != nil || got == nil {
		t.Fatal("the instance was there all along")
	}
}

func TestTransientFailuresThenSuccess(t *testing.T) {
	p, ctx := mk(Behaviour{TransientFailures: 2})
	var err error
	for i := 0; i < 3; i++ {
		_, err = p.Search(ctx, core.Criteria{})
		if err == nil {
			break
		}
		if !errs.Retryable(err) {
			t.Fatalf("attempt %d: class %s should be retryable", i, errs.ClassOf(err))
		}
	}
	if err != nil {
		t.Fatalf("should have succeeded by the third attempt: %v", err)
	}
}

func TestDestroyIsIdempotent(t *testing.T) {
	p, ctx := mk(Behaviour{})
	inst, _ := p.Create(ctx, offers()[0], provider.CreateSpec{Label: "01J9Z"})
	for i := 0; i < 3; i++ {
		if err := p.Destroy(ctx, inst.InstanceID); err != nil {
			t.Fatalf("destroy %d: %v", i, err)
		}
	}
	if p.Count() != 0 {
		t.Fatal("instance should be gone")
	}
	if got, _ := p.Get(ctx, inst.InstanceID); got != nil {
		t.Fatal("absence is the proof teardown needs")
	}
}
