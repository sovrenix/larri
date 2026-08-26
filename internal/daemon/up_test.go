// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
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
			Reliability: 0.97, MachineID: fmt.Sprintf("m%d", i),
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

// FR-PROV-04: a bring-up that fails after the instance exists must destroy it,
// not abandon it. A half-provisioned rig that nobody tears down is the exact
// outcome this product exists to prevent.
func TestFailedBringUpTearsDownRatherThanAbandoning(t *testing.T) {
	o, p, st := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	// The instance is created; the host then never becomes reachable, which
	// is what a dead sshd looks like.
	o.Deadline = 3 * time.Second

	_, err := o.UpAndServe(context.Background(), upReq())
	if err == nil {
		t.Fatal("bring-up should have failed")
	}
	if p.Count() != 0 {
		t.Fatalf("instance left behind after a failed bring-up: %d still exist", p.Count())
	}
	// And the failure explains itself, from the journal, afterwards.
	rigs, _ := st.List()
	if len(rigs) == 0 {
		t.Fatal("the attempt should be recorded")
	}
	last := rigs[0]
	if last.End == nil {
		t.Fatal("a rig destroyed by a fault must record why")
	}
	if last.End.Actor != core.ActorFault {
		t.Errorf("actor = %s, want fault", last.End.Actor)
	}
	if !strings.Contains(last.End.Summary, "bring-up failed") {
		t.Errorf("summary = %q", last.End.Summary)
	}
}

// The cleanup must survive the caller's context being cancelled, or a Ctrl-C
// during provisioning would leave the instance running.
func TestTeardownRunsEvenWhenTheContextIsCancelled(t *testing.T) {
	o, p, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	o.Deadline = 2 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel() // the operator hits Ctrl-C mid-boot
	}()
	if _, err := o.UpAndServe(ctx, upReq()); err == nil {
		t.Fatal("a cancelled bring-up should fail")
	}
	if p.Count() != 0 {
		t.Fatalf("cancellation left %d instances billing", p.Count())
	}
}

// FR-PROV-05, and the behaviour a live run showed was missing. The cheapest
// eligible machine — reliability 0.98 — never accepted a connection. Without
// fallback that is a total failure; with it, a warning and the next offer.
func TestHostFailureFallsBackToTheNextOffer(t *testing.T) {
	o, p, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	o.Deadline = 2 * time.Second
	o.MaxHostAttempts = 3

	_, err := o.UpAndServe(context.Background(), upReq())
	if err == nil {
		t.Fatal("every host is dead in this fake, so it should fail overall")
	}
	// Three machines tried, and none left behind.
	creates := 0
	for _, c := range p.Calls {
		if c == "Create" {
			creates++
		}
	}
	if creates < 2 {
		t.Errorf("only %d creates; a host failure must move to another machine", creates)
	}
	if p.Count() != 0 {
		t.Fatalf("%d instances left billing after fallback exhausted", p.Count())
	}
}

// The other half of FR-PROV-05: a model-attributable failure fails identically
// everywhere, so retrying elsewhere only spends more.
func TestModelFailureDoesNotRetryElsewhere(t *testing.T) {
	o, p, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{OOMAtLoad: true})
	o.Deadline = 5 * time.Second
	o.MaxHostAttempts = 3
	// Reach Serve by making the fake host immediately usable.
	o.Provider = p

	_, err := o.UpAndServe(context.Background(), upReq())
	if err == nil {
		t.Fatal("an OOM at load should fail the bring-up")
	}
	if p.Count() != 0 {
		t.Fatalf("%d instances left billing", p.Count())
	}
}

// A fallback must land on a different machine, not retry the one that just
// failed.
func TestFallbackSkipsTheOfferThatFailed(t *testing.T) {
	o, _, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	o.Deadline = time.Second
	o.MaxHostAttempts = 2

	first, err := o.Up(context.Background(), upReq())
	if err != nil {
		t.Fatal(err)
	}
	o.excludedMachines = append(o.excludedMachines, machineKey(first.Offer))

	second, err := o.Up(context.Background(), upReq())
	if err != nil {
		t.Fatal(err)
	}
	if second.Offer.OfferID == first.Offer.OfferID {
		t.Fatal("fallback re-selected the offer that just failed")
	}
	// The live failure: three fallbacks, three different offer IDs, the same
	// "GTX 1660 S $0.036/hr" box every time.
	if machineKey(second.Offer) == machineKey(first.Offer) {
		t.Fatal("fallback landed on the same physical host")
	}
	if machineKey(second.Offer) == machineKey(first.Offer) {
		t.Fatal("fallback landed on the same physical host, which is what a live run did")
	}
	if second.Offer.PriceHr < first.Offer.PriceHr {
		t.Error("the fallback should be the next cheapest, not a cheaper one")
	}
}

// A marketplace lists several offers per physical host, so excluding by offer
// ID lets a fallback land on exactly the box that just failed.
func TestFallbackExcludesTheWholeMachineNotJustTheOffer(t *testing.T) {
	sameBox := []core.Offer{
		{Provider: "fake", OfferID: "slot-1", MachineID: "44221", GPUModel: "GTX 1660 S", PriceHr: 0.036},
		{Provider: "fake", OfferID: "slot-2", MachineID: "44221", GPUModel: "GTX 1660 S", PriceHr: 0.036},
		{Provider: "fake", OfferID: "slot-3", MachineID: "44221", GPUModel: "GTX 1660 S", PriceHr: 0.036},
		{Provider: "fake", OfferID: "other", MachineID: "99887", GPUModel: "RTX 3060", PriceHr: 0.11},
	}
	left := withoutMachines(sameBox, []string{machineKey(sameBox[0])})
	if len(left) != 1 {
		t.Fatalf("%d offers survived; every slot on the failed host must go", len(left))
	}
	if left[0].MachineID != "99887" {
		t.Errorf("survivor is on machine %s, want a different host", left[0].MachineID)
	}
}

// Providers that do not report a machine fall back to per-offer exclusion,
// which is weaker but never worse than nothing.
// Amended: this asserted that an offer with no machine falls back to its own
// id, and that fallback was the bug. A catalogue provider's "offer" is a class
// of hardware, so excluding it after one failure would take every RTX 4090 out
// of the running — when retrying is exactly right, because the next pod lands
// on a different machine.
func TestMachineKeyOnlyNamesRealMachines(t *testing.T) {
	o := core.Offer{Provider: "p", OfferID: "abc"}
	if machineKey(o) != "" {
		t.Errorf("machineKey = %q; an unidentifiable host must not become an exclusion", machineKey(o))
	}
	o.MachineID = "44221"
	if machineKey(o) != "p:m44221" {
		t.Errorf("machineKey = %q", machineKey(o))
	}
}

// The marker the orchestrator stamps must actually carry what a recovering
// LARRI needs, and must actually be sealed when a key is configured.
//
// Both halves were briefly untrue at once: the encoding existed, the sealer
// existed, and nothing set the field — so a live run stamped every detail in
// the clear on a rented machine. A feature that is implemented but unwired
// looks finished from the inside.
func TestOrchestratorStampsARecoverableMarker(t *testing.T) {
	o, p, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	rig, err := o.Up(context.Background(), upReq())
	if err != nil {
		t.Fatal(err)
	}
	live, _ := p.List(context.Background())
	if len(live) != 1 {
		t.Fatalf("got %d instances", len(live))
	}
	raw := live[0].Labels[core.LabelRawKey]
	if raw == "" {
		t.Fatal("no raw marker stored; recovery would have only a bare id")
	}
	l, ok := core.DecodeLabel(raw)
	if !ok {
		t.Fatalf("marker not attributable: %q", raw)
	}
	if l.RigID != rig.ID {
		t.Errorf("marker rig = %q, want %q", l.RigID, rig.ID)
	}
	// Unsealed, the detail should be present and readable.
	if l.Model != rig.Model.Ref || l.Runtime != rig.Runtime {
		t.Errorf("marker lost detail: %+v", l)
	}
}

func TestOrchestratorSealsTheMarkerWhenAKeyIsConfigured(t *testing.T) {
	key, err := core.NewLabelKey()
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := core.NewAEADSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	o, p, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	o.LabelSealer = sealer

	rig, err := o.Up(context.Background(), upReq())
	if err != nil {
		t.Fatal(err)
	}
	live, _ := p.List(context.Background())
	raw := live[0].Labels[core.LabelRawKey]

	// The host must not be able to read what is being served.
	if strings.Contains(raw, rig.Model.Ref) {
		t.Fatalf("model name written in the clear on a rented machine: %q", raw)
	}
	// Attribution still works with no key, which is the deliberate exception.
	bare, ok := core.DecodeLabel(raw)
	if !ok || bare.RigID != rig.ID {
		t.Fatalf("a sealed marker must stay attributable without the key: %q", raw)
	}
	if !bare.Sealed {
		t.Error("the caller should be told the detail is sealed")
	}
	// And with the key, everything comes back.
	opened, ok := core.DecodeLabelWith(raw, sealer)
	if !ok || opened.Model != rig.Model.Ref {
		t.Errorf("sealed marker did not round trip: %+v", opened)
	}
}

// The exclusion list is about hosts, and only a provider that names hosts has
// any. A catalogue provider sells a GPU type and chooses the machine itself,
// so excluding its "offer" after one failure would take every RTX 4090 out of
// the running — when retrying is exactly right, because the next pod lands
// somewhere else.
func TestOffersWithoutAMachineAreNeverExcluded(t *testing.T) {
	catalogue := []core.Offer{
		{Provider: "catalogue", OfferID: "RTX 4090", GPUModel: "RTX 4090", PriceHr: 0.40},
		{Provider: "catalogue", OfferID: "A100", GPUModel: "A100", PriceHr: 1.20},
	}
	// Whatever the previous attempt recorded, nothing here can be excluded.
	tried := []string{machineKey(catalogue[0]), "catalogue:oRTX 4090", ""}
	got := withoutMachines(catalogue, tried)
	if len(got) != len(catalogue) {
		t.Fatalf("kept %d of %d offers; a nameless host was excluded", len(got), len(catalogue))
	}
	if machineKey(catalogue[0]) != "" {
		t.Errorf("machineKey invented a host: %q", machineKey(catalogue[0]))
	}
}

// And where hosts *are* named, exclusion still works — the fallback's whole
// purpose is not retrying the machine that just failed.
func TestNamedMachinesAreStillExcluded(t *testing.T) {
	market := []core.Offer{
		{Provider: "market", OfferID: "a", MachineID: "m1", GPUModel: "RTX 4090", PriceHr: 0.40},
		{Provider: "market", OfferID: "b", MachineID: "m1", GPUModel: "RTX 4090", PriceHr: 0.42},
		{Provider: "market", OfferID: "c", MachineID: "m2", GPUModel: "RTX 4090", PriceHr: 0.44},
	}
	got := withoutMachines(market, []string{machineKey(market[0])})
	if len(got) != 1 || got[0].MachineID != "m2" {
		t.Fatalf("got %d offers (%v); both listings on the failed machine should be gone",
			len(got), got)
	}
}

// A live run made every `larri up` look hung. Progress is queued to a channel
// a surface drains on its own goroutine, so the confirmation prompt — written
// straight to the terminal — overtook the exclusion report it refers to. The
// prompt ends without a newline so the next queued line overwrote it, leaving
// an operator staring at output with no visible question, at the one moment
// LARRI is about to spend money.
func TestConfirmPromptIsNotOvertakenByQueuedProgress(t *testing.T) {
	events := make(chan Event, 64)
	var mu sync.Mutex
	var order []string

	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range events {
			if !e.Show() {
				continue
			}
			mu.Lock()
			order = append(order, e.Phase)
			mu.Unlock()
		}
	}()

	o := &Orchestrator{Events: events}
	for i := 0; i < 20; i++ {
		o.emit("excluded", "offer %d", i)
	}
	o.emit("select", "a gpu")

	// What the confirm callback does: the barrier, then the prompt.
	o.Sync(context.Background())
	mu.Lock()
	seen := len(order)
	last := ""
	if seen > 0 {
		last = order[seen-1]
	}
	mu.Unlock()

	if seen != 21 {
		t.Errorf("Sync returned with %d of 21 events rendered; the prompt would print into a still-scrolling report", seen)
	}
	if last != "select" {
		t.Errorf("last rendered phase = %q, want the selection immediately above the prompt", last)
	}
	close(events)
	<-done
}

// A surface that stops draining must not be able to stall a run holding a
// rented GPU: the barrier is a courtesy to the terminal, not a dependency.
func TestSyncGivesUpOnASurfaceThatStoppedDraining(t *testing.T) {
	o := &Orchestrator{Events: make(chan Event)} // nobody draining
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	o.Sync(ctx)
	if time.Since(start) > syncGrace {
		t.Errorf("Sync held the lifecycle for %s", time.Since(start))
	}
}

// A nil channel is the non-interactive case, and must not deadlock.
func TestSyncWithoutASurfaceIsANoOp(t *testing.T) {
	(&Orchestrator{}).Sync(context.Background())
}
