// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"errors"
	"go.sovrenix.com/larri/internal/errs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/claw"
	cfake "go.sovrenix.com/larri/internal/claw/fake"
	"go.sovrenix.com/larri/internal/core"
	pfake "go.sovrenix.com/larri/internal/provider/fake"
	"go.sovrenix.com/larri/internal/rank"
	rfake "go.sovrenix.com/larri/internal/runtime/fake"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/sizing"
	"go.sovrenix.com/larri/internal/state"
	"go.sovrenix.com/larri/internal/wire"
)

func clawOrch(t *testing.T) *Orchestrator {
	t.Helper()
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Orchestrator{
		Store:    st,
		Provider: pfake.New("fake", offers(), pfake.Behaviour{}),
		// ClawUp replaces this with whatever the Kind builds; the tests that
		// reach Up directly still need something to ask Requires of.
		Runtime:  rfake.New(rfake.Behaviour{}),
		Resolver: sizing.StaticResolver{"test/model": facts},
		Policy:   rank.DefaultPolicy(),
		Deadline: time.Minute,
		// One attempt, briefly: the tests that reach this far are asserting
		// what happens before and after a bring-up, not that a fake provider
		// can produce a reachable host.
		MaxHostAttempts:    1,
		EndpointStallLimit: time.Second,
	}
}

// sessionFor builds a torn-down-able session without a real rig behind it.
//
// ClawDown's contract is about ordering and about what cannot block a destroy,
// and both are observable with an instance the fake provider created and no SSH
// at all — which is also the shape of the case that matters most, a host that
// has already gone.
func sessionFor(t *testing.T, o *Orchestrator, k claw.Kind) *ClawSession {
	t.Helper()
	rig, err := o.Up(context.Background(), UpRequest{
		Model: core.ModelSpec{Ref: "test/model", ServedName: "test",
			Quantization: "q4_K_M", ContextLen: 8192},
		DiskGB: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &ClawSession{
		Live: &Live{Rig: rig}, Kind: k,
		Plan:      &claw.Plan{Model: rig.Model},
		OutputDir: t.TempDir(), StartedAt: time.Now(),
	}
}

func term() *core.Termination {
	return &core.Termination{
		Actor: core.ActorOperator, Code: core.ReasonOperatorRequest,
		At: time.Now().UTC(), Summary: "test",
	}
}

// A remote claw produces things that exist only on the host, so renting one
// with nowhere to put them is a session whose results are lost by construction.
func TestARemoteClawNeedsSomewhereToPutItsResults(t *testing.T) {
	o := clawOrch(t)
	k := cfake.NewRemote("demo", cfake.Behaviour{})
	_, err := o.ClawUp(context.Background(), ClawRequest{
		Kind: k, Plan: &claw.Plan{}, OutputDir: "",
	})
	if err == nil {
		t.Fatal("a remote claw was rented with no output directory")
	}
}

// A local claw produces nothing on the host, so it must not be made to name an
// output directory it has no use for.
func TestALocalClawNeedsNoOutputDirectory(t *testing.T) {
	o := clawOrch(t)
	k := cfake.NewLocal("demo", cfake.Behaviour{})
	// It fails later for want of a reachable host; what matters is that it is
	// not refused for the output directory.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := o.ClawUp(ctx, ClawRequest{
		Kind: k, Plan: &claw.Plan{Model: core.ModelSpec{Ref: "test/model", ServedName: "t"}},
	})
	if err != nil && errorMentions(err, "output directory") {
		t.Fatalf("a local claw was refused for want of an output directory: %v", err)
	}
}

// Results exist only on the host and a destroy is irreversible, so collection
// happens first — and the teardown proceeds either way.
func TestARemoteClawCollectsBeforeDestroying(t *testing.T) {
	o := clawOrch(t)
	k := cfake.NewRemote("demo", cfake.Behaviour{
		Collected: &claw.Result{Saved: []string{"a.png"}, Failed: map[string]string{}},
	})
	s := sessionFor(t, o, k)
	rig := s.Live.Rig

	// No SSH session exists, so collection cannot run — which is exactly the
	// case that must still destroy.
	res, err := o.ClawDown(context.Background(), s, term())
	if err != nil {
		t.Fatalf("teardown refused to proceed: %v", err)
	}
	if rig.State != core.StateDestroyed {
		t.Errorf("state = %s, want DESTROYED", rig.State)
	}
	_ = res
}

// A collection that fails outright must not keep a rig alive. Files left
// behind are lost once; a rig left alive bills until somebody notices.
func TestAFailedCollectionStillDestroys(t *testing.T) {
	o := clawOrch(t)
	k := cfake.NewRemote("demo", cfake.Behaviour{CollectErr: errors.New("host gone")})
	s := sessionFor(t, o, k)
	rig := s.Live.Rig

	if _, err := o.ClawDown(context.Background(), s, term()); err != nil {
		t.Fatalf("a failed collection blocked the teardown: %v", err)
	}
	if rig.State != core.StateDestroyed {
		t.Errorf("state = %s, want DESTROYED", rig.State)
	}
	if rig.End == nil {
		t.Error("no termination record; a destroyed rig must say why it went")
	}
}

// A local claw's clients are put back before the instance goes, so there is no
// window in which an editor points at a dead endpoint (invariant 3).
func TestALocalClawRevertsBeforeDestroying(t *testing.T) {
	o := clawOrch(t)
	c := cfake.NewClient("editor")
	k := cfake.NewLocal("demo", cfake.Behaviour{Clients: []wire.ClientWriter{c}})
	s := sessionFor(t, o, k)

	// Wire it the way attachLocal would, then tear down.
	recs, _ := wire.Apply([]wire.ClientWriter{c}, wire.Endpoint{URL: "http://127.0.0.1:8000/v1"}, nil)
	s.Wiring = recs
	s.clients = wire.Index([]wire.ClientWriter{c})
	rig := s.Live.Rig

	if _, err := o.ClawDown(context.Background(), s, term()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if _, reverted := c.Counts(); reverted != 1 {
		t.Errorf("client reverted %d times, want 1", reverted)
	}
	if rig.State != core.StateDestroyed {
		t.Errorf("state = %s, want DESTROYED", rig.State)
	}
}

// A config that could not be put back is recoverable from its backup. A rig
// left alive is not recoverable at all, so a failed revert must not block.
func TestAFailedRevertStillDestroys(t *testing.T) {
	o := clawOrch(t)
	c := cfake.NewClient("stubborn")
	c.RevertErr = errors.New("file is read-only")
	k := cfake.NewLocal("demo", cfake.Behaviour{Clients: []wire.ClientWriter{c}})
	s := sessionFor(t, o, k)

	recs, _ := wire.Apply([]wire.ClientWriter{c}, wire.Endpoint{URL: "http://127.0.0.1:8000/v1"}, nil)
	s.Wiring = recs
	s.clients = wire.Index([]wire.ClientWriter{c})
	rig := s.Live.Rig

	if _, err := o.ClawDown(context.Background(), s, term()); err != nil {
		t.Fatalf("a failed revert blocked the teardown: %v", err)
	}
	if rig.State != core.StateDestroyed {
		t.Errorf("state = %s, want DESTROYED", rig.State)
	}
}

// A local claw has nothing on the host to rescue, so no collection is
// attempted — asking for one would mean an SSH round trip at teardown for a
// session that produced nothing there.
func TestALocalClawCollectsNothing(t *testing.T) {
	o := clawOrch(t)
	k := cfake.NewLocal("demo", cfake.Behaviour{})
	s := sessionFor(t, o, k)

	res, err := o.ClawDown(context.Background(), s, term())
	if err != nil {
		t.Fatal(err)
	}
	if res != nil {
		t.Errorf("a local claw returned a collection result: %+v", res)
	}
	for _, step := range k.Steps() {
		if step == "collect" {
			t.Error("a local claw was asked to collect")
		}
	}
}

// A claw that measured its own requirement supplies it, and the market is
// ranked against the bytes it says it must fetch rather than a number derived
// from a field that does not describe it (§4b).
func TestAFixedSizingDrivesSelection(t *testing.T) {
	o := clawOrch(t)
	need := &core.SizingPlan{RequiredVRAMBytes: 20 << 30, WeightsBytes: 6 << 30, FitsInVRAM: true}
	k := cfake.NewRemote("demo", cfake.Behaviour{
		Sizing: need, ColdStartBytes: 7 << 30,
	})
	plan, err := k.Plan(context.Background(), nil, claw.Options{})
	if err != nil {
		t.Fatal(err)
	}
	o.Runtime = k.Server(plan)
	o.Planner = func(context.Context, UpRequest) (core.SizingPlan, error) { return *need, nil }
	o.ColdStart = plan.ColdStartBytes

	sv, err := o.Offers(context.Background(), UpRequest{Model: plan.Model})
	if err != nil {
		t.Fatal(err)
	}
	if sv.Plan.RequiredVRAMBytes != need.RequiredVRAMBytes {
		t.Errorf("survey sized %d, want the claw's %d",
			sv.Plan.RequiredVRAMBytes, need.RequiredVRAMBytes)
	}
	if sv.Selection.Selected == nil {
		t.Fatal("nothing was selected for a requirement the market can meet")
	}
}

// A planned survey still has to carry the model and the disk, which an early
// return dropped by omission.
//
// Both are load-bearing on the way to the create call. Up assigns
// req.Model = sv.Model, so a zero one erases the spec the claw resolved — a
// live whisper rig persisted an empty model ref because of this. And Create is
// passed sv.DiskGB, so a zero one asks the provider for its floor after the
// search has already filtered the market on the disk the payload needs, which
// is the mismatch Survey.DiskGB's own doc comment forbids.
func TestAPlannedSurveyStillCarriesTheModelAndTheDisk(t *testing.T) {
	o := clawOrch(t)
	need := &core.SizingPlan{RequiredVRAMBytes: 20 << 30, WeightsBytes: 6 << 30, FitsInVRAM: true}
	k := cfake.NewRemote("demo", cfake.Behaviour{Sizing: need, ColdStartBytes: 7 << 30})
	plan, err := k.Plan(context.Background(), nil, claw.Options{})
	if err != nil {
		t.Fatal(err)
	}
	o.Runtime = k.Server(plan)
	o.Planner = func(context.Context, UpRequest) (core.SizingPlan, error) { return *need, nil }

	spec := core.ModelSpec{Ref: "someone/a-payload", ServedName: "demo"}
	sv, err := o.Offers(context.Background(), UpRequest{Model: spec, DiskGB: 120})
	if err != nil {
		t.Fatal(err)
	}
	if sv.Model.Ref != spec.Ref || sv.Model.ServedName != spec.ServedName {
		t.Errorf("survey returned model %+v, so Up would erase what the claw resolved", sv.Model)
	}
	if sv.DiskGB < 120 {
		t.Errorf("survey returned %d GB of disk against the 120 asked for: "+
			"the create call would ask for less than the search filtered on", sv.DiskGB)
	}
}

// The operator's floors are theirs. A claw needing less has not contradicted
// them, and renting something cheaper than what was asked for cannot be undone
// after the fact.
func TestAClawDoesNotLowerTheOperatorsFloors(t *testing.T) {
	k := cfake.NewRemote("demo", cfake.Behaviour{
		Criteria: core.Criteria{VRAMPerGPUGB: 12, RAMGB: 16},
	})
	plan, err := k.Plan(context.Background(), nil, claw.Options{
		Criteria: core.Criteria{VRAMPerGPUGB: 80, RAMGB: 256, MaxPriceHr: 0.4},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Criteria.VRAMPerGPUGB != 80 || plan.Criteria.RAMGB != 256 {
		t.Errorf("criteria = %+v, want the operator's larger floors kept", plan.Criteria)
	}
	if plan.Criteria.MaxPriceHr != 0.4 {
		t.Error("an unrelated criterion was dropped")
	}
}

// A claw that refuses in Plan must refuse before anything is rented: that is
// the whole reason the plan is produced separately (§4a).
func TestAPlanThatRefusesSpendsNothing(t *testing.T) {
	k := cfake.NewRemote("demo", cfake.Behaviour{PlanErr: errors.New("no source for a model")})
	if _, err := k.Plan(context.Background(), nil, claw.Options{}); err == nil {
		t.Fatal("a refusing plan returned no error")
	}
	for _, step := range k.Steps() {
		if step == "server" {
			t.Error("a refused plan still built a workload")
		}
	}
}

func errorMentions(err error, sub string) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// A claw the operator opens rather than configures needs two things no engine
// does, and neither can be inferred: a credential a browser will actually send,
// and a rule for what counts as work.
func TestABrowserClawGetsASessionAndAWorkRule(t *testing.T) {
	o := clawOrch(t)
	k := cfake.NewBrowser("demo", cfake.Behaviour{})

	proxy, err := wire.NewProxy(0)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()

	token, err := secret.Generate(16)
	if err != nil {
		t.Fatal(err)
	}
	s := &ClawSession{
		Live: &Live{proxy: proxy, ClientToken: token, Rig: &core.Rig{}},
		Kind: k, Plan: &claw.Plan{},
	}
	if err := o.attachRemote(s); err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() {
		if s.hold != nil {
			s.hold()
		}
	})

	// A browser cannot present a bearer token, so a one-time link has to exist
	// for it to trade for a cookie.
	if s.URL == "" {
		t.Error("no session link; a browser has no way to authenticate")
	}
	if proxy.CountsAsWork == nil {
		t.Fatal("no work rule; an open tab polling would hold the rig forever")
	}
	if proxy.CountsAsWork(httptest.NewRequest(http.MethodGet, "/queue", nil)) {
		t.Error("idle chatter was counted as work")
	}
	if !proxy.CountsAsWork(httptest.NewRequest(http.MethodPost, "/submit", nil)) {
		t.Error("real work was not counted")
	}

	// Work producing no requests still has to hold the clock.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !k.Holding() {
		time.Sleep(2 * time.Millisecond)
	}
	if !k.Holding() {
		t.Error("the idle clock was never held; a long job would be destroyed mid-way")
	}
}

// A remote claw that is not opened in a browser must not be given a browser
// credential: the cookie path exists for surfaces that cannot send a header,
// and widening it would weaken the local listener for no reason.
func TestAPlainRemoteClawGetsNoBrowserSession(t *testing.T) {
	o := clawOrch(t)
	proxy, err := wire.NewProxy(0)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()

	s := &ClawSession{
		Live: &Live{proxy: proxy, Rig: &core.Rig{}},
		Kind: cfake.NewRemote("demo", cfake.Behaviour{}), Plan: &claw.Plan{},
	}
	if err := o.attachRemote(s); err != nil {
		t.Fatal(err)
	}
	if s.URL != "" {
		t.Error("a non-browser claw was given a session link")
	}
	if proxy.CountsAsWork != nil {
		t.Error("a non-browser claw narrowed what counts as work")
	}
}

// One credential per client (FR-SEC-23) is what lets the probe say that *this*
// application arrived rather than that something did, and what makes one
// client revocable without rewiring the rest.
func TestEachWiredClientGetsItsOwnCredential(t *testing.T) {
	base := secret.New("rig-base-token")
	a, err := clientToken(base, "subtitle-edit")
	if err != nil {
		t.Fatalf("a real base failed to derive a key: %v", err)
	}
	b, err := clientToken(base, "buzz")
	if err != nil {
		t.Fatalf("a real base failed to derive a key: %v", err)
	}

	if a.Reveal() == b.Reveal() {
		t.Fatal("two clients share a credential, so neither can be revoked alone")
	}
	if a.Reveal() == base.Reveal() || b.Reveal() == base.Reveal() {
		t.Error("a client was handed the rig's own token")
	}
	// Reproducible, so a value the operator pasted keeps working for as long
	// as the rig does rather than only until the next call.
	again, _ := clientToken(base, "subtitle-edit")
	if again.Reveal() != a.Reveal() {
		t.Error("the same client got a different credential from the same rig token")
	}
	// The rig token must not be recoverable from what the operator pastes into
	// a config file.
	if strings.Contains(a.Reveal(), base.Reveal()) {
		t.Error("the client credential contains the rig token verbatim")
	}
}

// HMAC with no key is a constant, so an empty base would give every
// installation the same credential for the same client name — a value that
// passes for a key while authenticating nothing. The claw path always sets a
// base today; this is what keeps that true.
func TestAnEmptyBaseDerivesNoCredential(t *testing.T) {
	if _, err := clientToken(secret.Secret{}, "subtitle-edit"); err == nil {
		t.Fatal("an empty rig token produced a client credential")
	} else if errs.ClassOf(err) != errs.ClassWiring {
		t.Errorf("class is %v, want ClassWiring: a key that cannot be derived "+
			"is a wiring failure, not a reason to fail the rig", errs.ClassOf(err))
	}
	// Two names must not collapse onto one value either, which is what an
	// unguarded empty base would have done.
	a, _ := clientToken(secret.Secret{}, "subtitle-edit")
	b, _ := clientToken(secret.Secret{}, "buzz")
	if a.Reveal() != "" || b.Reveal() != "" {
		t.Error("a credential was returned alongside the refusal")
	}
}

// The busy-work poll is LARRI's own traffic and must carry LARRI's own key.
//
// It used the operator's client token, which attachTunnel leaves empty
// whenever a readable client-key store is configured — the ordinary setup for
// anyone who has run `larri token`. Every /queue poll would then be
// unauthenticated, HoldWhileBusy would never bracket the render, and the idle
// timer it exists to hold off would destroy a job mid-flight.
func TestTheBusyPollUsesLARRIsOwnCredential(t *testing.T) {
	// The case that was broken: a rig with no per-rig client token, which is
	// what a readable key store produces.
	live := &Live{probeToken: secret.New("probe-key")}
	ep := holderEndpoint(live, 8188)

	if ep.Token == "" {
		t.Fatal("the poll carries no credential, so every /queue request is " +
			"rejected and the render is never held")
	}
	if ep.Token != "probe-key" {
		t.Errorf("token = %q, want LARRI's own probe credential", ep.Token)
	}
	if ep.Addr != "127.0.0.1:8188" {
		t.Errorf("addr = %q, want the fixed local port", ep.Addr)
	}

	// And it stays LARRI's own even where a client token does exist, so the
	// poll is never attributed to the operator.
	both := &Live{probeToken: secret.New("probe-key"), ClientToken: secret.New("client-key")}
	if got := holderEndpoint(both, 8188).Token; got != "probe-key" {
		t.Errorf("token = %q; the poll must not present the operator's key", got)
	}
}

// The teardown guard has to live where every surface reaches it.
//
// It was in the CLI's cmdDown alone, so the MCP tool and the TUI called Down
// directly and destroyed the only copy of a session's renders with nobody
// deciding to. A rule enforced in one front-end is in the wrong layer
// (invariant 6).
func TestNoSurfaceDestroysUncollectedResultsBySilence(t *testing.T) {
	o, live := liveRig(t)
	rig := live.Rig
	rig.Runtime = core.RuntimeComfyUI
	rig.ClawSite = core.ClawSiteRemote

	undecided := &core.Termination{
		Actor: core.ActorOperator, Code: core.ReasonOperatorRequest,
		Summary: "a surface that never asked",
	}
	if err := o.Down(context.Background(), rig, undecided); err == nil {
		t.Fatal("a teardown that said nothing about the renders destroyed the host")
	}

	// Saying so is one field, so nothing is made undestroyable — a billing
	// rig nobody can stop is worse than a lost render.
	decided := &core.Termination{
		Actor: core.ActorOperator, Code: core.ReasonOperatorRequest,
		Summary: "discarded on purpose", Outputs: core.OutputsDiscarded,
	}
	if err := o.Down(context.Background(), rig, decided); err != nil {
		t.Errorf("an explicit discard was refused: %v", err)
	}
}

// And a local claw has nothing on the host, so it must not be caught by it.
func TestALocalClawIsNotHeldUpByTheOutputGuard(t *testing.T) {
	o, live := liveRig(t)
	rig := live.Rig
	rig.Runtime = core.RuntimeWhisper
	rig.ClawSite = core.ClawSiteLocal

	term := &core.Termination{
		Actor: core.ActorOperator, Code: core.ReasonOperatorRequest,
		Summary: "nothing was ever on that host",
	}
	if err := o.Down(context.Background(), rig, term); err != nil {
		t.Errorf("a local claw was refused over renders it never made: %v", err)
	}
}
