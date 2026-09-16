// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/config"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/provider"
	"go.sovrenix.com/larri/internal/rank"
	"go.sovrenix.com/larri/internal/state"
)

// The guard that keeps a dead process from turning into two invoices. An
// operator whose daemon died reaching for `up` again is the likeliest way to
// end up paying for two rigs, and the second one is invisible until it is
// billed.
func TestUpRefusesWhileARigIsStillBilling(t *testing.T) {
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	rig := &core.Rig{ID: newTestID(t), Offer: core.Offer{PriceHr: 0.44}}
	if err := st.Transition(rig, core.StateReady, "test"); err != nil {
		t.Fatal(err)
	}

	err = refuseIfAlreadyBilling(st)
	if err == nil {
		t.Fatal("a second rig would have been rented alongside a billing one")
	}
	// The message has to carry the way out, or it is an obstacle rather than
	// a safeguard.
	for _, want := range []string{rig.ID, "larri resume", "larri down", "0.440"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message should mention %q, got:\n%s", want, err)
		}
	}
}

// A destroyed rig is not a reason to refuse; the guard must not make the tool
// unusable after a normal teardown.
func TestUpProceedsWhenNothingIsBilling(t *testing.T) {
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	rig := &core.Rig{ID: newTestID(t)}
	if err := st.Transition(rig, core.StateDestroyed, "test"); err != nil {
		t.Fatal(err)
	}
	if err := refuseIfAlreadyBilling(st); err != nil {
		t.Errorf("refused with nothing billing: %v", err)
	}
}

func newTestID(t *testing.T) string {
	t.Helper()
	id, err := state.NewID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The bug this pins: a live run against 1376 offers printed one row and
// presented it as the market, because the filter tested for ReasonEligible
// when every runner-up is marked ReasonCostlier.
func TestOffersListsRunnersUpNotJustTheWinner(t *testing.T) {
	sel := rank.Result{
		Candidates: []rank.Candidate{
			{Offer: core.Offer{OfferID: "a", PriceHr: 0.03}, Reason: rank.ReasonEligible},
			{Offer: core.Offer{OfferID: "b", PriceHr: 0.04}, Reason: rank.ReasonCostlier},
			{Offer: core.Offer{OfferID: "c", PriceHr: 0.05}, Reason: rank.ReasonCostlier},
			{Offer: core.Offer{OfferID: "d", PriceHr: 0.01}, Reason: rank.ReasonPriceOutlier},
		},
	}
	got := eligibleTop(sel, 10)
	if len(got) != 3 {
		t.Fatalf("showed %d offers, want the winner plus 2 runners-up", len(got))
	}
	if got[0].Offer.OfferID != "a" {
		t.Errorf("not cheapest-first: %s", got[0].Offer.OfferID)
	}
	for _, c := range got {
		if c.Offer.OfferID == "d" {
			t.Error("an excluded outlier was offered as rentable")
		}
	}
	if n := len(eligibleTop(sel, 2)); n != 2 {
		t.Errorf("--top not honoured: got %d", n)
	}
}

// The README is a public promise, and it was wrong in both directions once:
// it said there was no working code while LARRI rented GPUs, and it advertised
// seven commands that did not exist. Documentation drift is not a
// documentation problem when the document is how people decide whether to
// trust the thing with their money.
func TestEveryCommandTheReadmeAdvertisesExists(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Skipf("README not readable from here: %v", err)
	}
	main, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	dispatch := string(main)

	re := regexp.MustCompile("(?m)^\\| `larri ([a-z]+)`")
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(readme), -1) {
		cmd := m[1]
		if seen[cmd] {
			continue
		}
		seen[cmd] = true
		if !strings.Contains(dispatch, `case "`+cmd+`"`) {
			t.Errorf("README advertises `larri %s`, which the CLI does not dispatch", cmd)
		}
	}
	if len(seen) == 0 {
		t.Error("no commands found in the README table; the guard has stopped guarding")
	}
}

// The converse: a command nobody documented is a command nobody finds.
func TestEveryCommandIsDocumentedInTheUsage(t *testing.T) {
	main, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(main)
	re := regexp.MustCompile(`(?m)^\tcase "([a-z]+)":`)
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		cmd := m[1]
		if cmd == "version" || cmd == "help" {
			continue // listed in usage under their own wording
		}
		if !strings.Contains(usage, "larri "+cmd) {
			t.Errorf("`larri %s` is dispatched but absent from the usage text", cmd)
		}
	}
}

// FR-CFG-02, enforced by inspection because the failure is invisible in
// testing and catastrophic in use: a stdio protocol server that opens a
// terminal prompt hands its host a process that produces no output and never
// exits. Invocation.MCP exists for exactly this and was set nowhere in the
// codebase until first-run config made it matter.
func TestMCPDeclaresItselfNonInteractive(t *testing.T) {
	src, err := os.ReadFile("mcp.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "MCP: true") {
		t.Error("cmdMCP does not declare itself non-interactive, so it may be prompted")
	}
}

// Configuration creation must never depend on a terminal. EnsureExists writes
// and returns; nothing in the spending path may open a form.
func TestSpendingCommandsNeverOpenAForm(t *testing.T) {
	for _, f := range []string{"main.go", "tui.go", "mcp.go", "inspect.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "tui.NewEditor") {
			t.Errorf("%s opens the criteria editor; editing belongs in `larri config`", f)
		}
	}
}

// `up` and `config` must agree on which profiles exist. They did not: `up
// --profile codr` errored while `config --profile codr` silently created a
// phantom, so a typo produced a profile that only revealed itself as missing
// from the other command.
func TestConfigRefusesAnUnknownProfileLikeUpDoes(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	cfg := config.Default()
	cfg.Profiles = map[string]config.Profile{"coder": {Model: "org/m"}}
	if err := config.Save(file, cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LARRI_CONFIG", file)

	err := cmdConfig(context.Background(), []string{"--profile", "codr"})
	if err == nil {
		t.Fatal("config accepted a profile name that does not exist")
	}
	if !strings.Contains(err.Error(), "--new") {
		t.Errorf("the error should say how to create one: %v", err)
	}

	// And `up` must refuse the same name, for the same reason.
	if _, rerr := config.Resolve(config.Request{Path: file, Profile: "codr"}); rerr == nil {
		t.Error("resolution accepted the same unknown profile")
	}
}

// Friction is a security property here, not a cosmetic one: an operator who
// finds LARRI harder than the provider's own one-click deploy will use the
// one-click deploy — which publishes the inference port behind a fixed token
// and makes no teardown guarantee. So refusing to choose between two
// configured providers was not caution; it made --provider mandatory the
// moment a second provider was compiled in, and the configuration already
// answers the question.
func TestProviderResolvesWithoutBeingNamed(t *testing.T) {
	names := provider.Names()
	if len(names) < 2 {
		t.Skipf("only %d provider(s) compiled in; nothing to choose between", len(names))
	}
	// provider.Default deliberately still refuses — the ambiguity is real and
	// the registry is not the place that resolves it.
	if _, err := provider.Default(); err == nil {
		t.Error("registry Default should stay strict about ambiguity")
	}
	// The CLI resolves it, or reports why it cannot — never "name one".
	p, err := openProvider("")
	if err != nil {
		if strings.Contains(err.Error(), "--provider") {
			t.Errorf("the operator should not be told to supply a flag the config answers: %v", err)
		}
		t.Skipf("no provider credential available in this environment: %v", err)
	}
	if p == nil {
		t.Fatal("resolved a nil provider")
	}
	// An explicit name still wins.
	if _, err := openProvider(names[0]); err != nil {
		t.Logf("explicit %s unavailable here: %v", names[0], err)
	}
}

// Progress and the confirmation prompt used to reach the terminal by
// different routes — progress through a channel drained on a goroutine, the
// question written directly by the lifecycle. A live run printed the question
// into the middle of the exclusion report it referred to, and the next queued
// line wrote over it: no visible prompt, at the one moment LARRI was about to
// spend money.
func TestPromptPrintsAfterEverythingQueuedBeforeIt(t *testing.T) {
	events := make(chan daemon.Event, 64)
	prompts := make(chan cliPrompt)
	var out bytes.Buffer

	done := make(chan struct{})
	go func() { defer close(done); renderCLI(&out, strings.NewReader("y\n"), events, prompts) }()

	// Everything a survey emits before it asks.
	for i := 0; i < 12; i++ {
		events <- daemon.Event{Phase: "excluded", Message: fmt.Sprintf("offer %d", i)}
	}
	events <- daemon.Event{Phase: "select", Message: "vastai RTX 3060 12GB"}

	// The lifecycle asks, exactly as Confirm does.
	result := make(chan bool)
	prompts <- cliPrompt{Offer: core.Offer{GPUModel: "RTX 3060", VRAMPerGPUGB: 12, GPUCount: 1, PriceHr: 0.047}, Result: result}
	if !<-result {
		t.Error(`"y" should confirm`)
	}
	close(events)
	<-done

	got := out.String()
	promptAt := strings.Index(got, "rent RTX 3060")
	if promptAt < 0 {
		t.Fatalf("no prompt in output:\n%s", got)
	}
	// Every queued line must already be on screen when the question appears.
	for _, must := range []string{"offer 0", "offer 11", "vastai RTX 3060 12GB"} {
		at := strings.Index(got, must)
		if at < 0 {
			t.Errorf("%q never printed", must)
		} else if at > promptAt {
			t.Errorf("%q printed after the prompt; the question would land mid-report", must)
		}
	}
	// And nothing follows the question, so the cursor stays on it.
	if tail := strings.TrimSpace(got[promptAt:]); !strings.HasSuffix(tail, "[y/N]") {
		t.Errorf("output continued past the prompt: %q", tail)
	}
}

// "n" declines, and anything else declines too — the flag is [y/N].
func TestPromptDefaultsToDeclining(t *testing.T) {
	for _, answer := range []string{"n\n", "\n", "maybe\n"} {
		events := make(chan daemon.Event, 4)
		prompts := make(chan cliPrompt)
		go renderCLI(io.Discard, strings.NewReader(answer), events, prompts)
		result := make(chan bool)
		prompts <- cliPrompt{Offer: core.Offer{GPUModel: "X"}, Result: result}
		if <-result {
			t.Errorf("%q should not confirm a purchase", answer)
		}
		close(events)
	}
}

// `larri status` answers what an operator otherwise opens a provider
// dashboard for: where, what hardware, what the provider calls it, what it
// costs by the hour — and both rates when the bill differs from the quote.
func TestStatusShowsProviderHardwareInstanceAndRate(t *testing.T) {
	var b strings.Builder
	printRig(&b, state.Summary{
		ID: "r1", State: core.StateReady, Provider: "runpod",
		Hardware: "2× A100 SXM 160GB", Instance: "pod1",
		PriceHr: 3.18, QuotedHr: 2.78, Model: "org/m", Quantization: "Q4_K_M",
		Runtime: core.RuntimeLlamaCpp, Served: "m",
		Endpoint: "http://127.0.0.1:8000/v1",
	})
	out := b.String()
	for _, want := range []string{
		"runpod · 2× A100 SXM 160GB · $3.180/hr (quoted $2.780)",
		"instance  pod1",
		"org/m @ Q4_K_M · llamacpp · served as m",
		"endpoint  http://127.0.0.1:8000/v1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}

// With no instance on record the line says so, as a statement of what LARRI
// knows rather than a claim that the provider has nothing.
func TestStatusSaysWhenNoInstanceIsRecorded(t *testing.T) {
	var b strings.Builder
	printRig(&b, state.Summary{ID: "r1", State: core.StateSelected,
		Provider: "runpod", Hardware: "A100 SXM 80GB", PriceHr: 1.39, QuotedHr: 1.39})
	out := b.String()
	if !strings.Contains(out, "instance  none recorded") {
		t.Errorf("no-instance rig not labelled:\n%s", out)
	}
	if strings.Contains(out, "quoted") {
		t.Errorf("a quote equal to the rate was shown as a difference:\n%s", out)
	}
}

// A billing rig says who holds it, and a detached holder where its log is. One
// nobody holds is the state a rig is in after its holder died: billing, with no
// endpoint and nothing reclaiming it, and the line says so. A failed rig is
// not one to reconnect to, so it is not offered.
func TestStatusSaysWhoHoldsABillingRig(t *testing.T) {
	started := time.Date(2026, 9, 14, 9, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		s    state.Summary
		want []string
		not  []string
	}{
		{"detached", state.Summary{ID: "r1", State: core.StateReady, Held: true,
			Holder: state.Holder{PID: 77, Started: started, Detached: true, Log: "/s/logs/up.log"}},
			[]string{"held      by larri pid 77 since " + started.Local().Format("15:04"), "(detached; log /s/logs/up.log)"}, nil},
		{"orphaned", state.Summary{ID: "r2", State: core.StateReady},
			[]string{"no endpoint, no idle reclamation", "larri resume r2, or larri down r2"}, nil},
		{"failed", state.Summary{ID: "r3", State: core.StateFailed},
			[]string{"nothing supervises it: larri down r3"}, []string{"resume"}},
		{"destroyed", state.Summary{ID: "r4", State: core.StateDestroyed}, nil, []string{"held"}},
	} {
		var b strings.Builder
		printRig(&b, c.s)
		out := b.String()
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: status lacks %q:\n%s", c.name, w, out)
			}
		}
		for _, n := range c.not {
			if strings.Contains(out, n) {
				t.Errorf("%s: status says %q:\n%s", c.name, n, out)
			}
		}
	}
}

// The local port resolves like every other setting — flag, then profile, then
// file — and the file's is not ignored.
func TestTheLocalPortComesFromTheFileWhenNothingElseNamesOne(t *testing.T) {
	file := config.Config{LocalPort: 8123}
	for _, c := range []struct {
		name string
		set  map[string]bool
		p    config.Profile
		flag int
		want int
	}{
		{"file", map[string]bool{}, config.Profile{}, 8000, 8123},
		{"flag", map[string]bool{"port": true}, config.Profile{}, 9000, 9000},
		{"profile", map[string]bool{}, config.Profile{LocalPort: 8200}, 8200, 8200},
	} {
		if got := localPort(c.set, c.p, file, c.flag); got != c.want {
			t.Errorf("%s: port %d, want %d", c.name, got, c.want)
		}
	}
	if got := localPort(map[string]bool{}, config.Profile{}, config.Config{}, 8000); got != 8000 {
		t.Errorf("no file port: %d, want the flag default", got)
	}
}

func TestServedNameForAFileRefIsTheModelNotTheFile(t *testing.T) {
	for ref, want := range map[string]string{
		"Qwen/Qwen3-8B":                            "qwen3-8b",
		"unsloth/Qwen3-8B-GGUF":                    "qwen3-8b-gguf",
		"unsloth/Qwen3-8B-GGUF/Qwen3-8B-Q8_0.gguf": "qwen3-8b-q8_0",
		"unsloth/Llama-3.3-70B-Instruct-GGUF/Q6_K/Llama-3.3-70B-Instruct-Q6_K-00001-of-00002.gguf": "llama-3.3-70b-instruct-q6_k",
		"llama3.1:70b": "llama3.1:70b",
	} {
		if got := servedNameFor(ref); got != want {
			t.Errorf("servedNameFor(%q) = %q, want %q", ref, got, want)
		}
	}
}

// `larri resume` reconnects with the engine the rig was brought up with. It
// used vLLM for every rig, and each engine finds its own server process and
// port, so a llama.cpp or Ollama rig could not be reconnected to.
func TestAResumedRigGetsItsOwnEngine(t *testing.T) {
	for _, kind := range []core.RuntimeKind{core.RuntimeLlamaCpp, core.RuntimeOllama, core.RuntimeVLLM} {
		eng, err := pickRuntime(string(kind), core.ModelSpec{Ref: "org/model"})
		if err != nil {
			t.Fatal(err)
		}
		if eng.Kind() != kind {
			t.Errorf("rig on %s resumed with %s", kind, eng.Kind())
		}
	}
}

// A profile's low-stock choice applies unless the command line says
// otherwise, the rule every other profile setting follows.
func TestAProfileAllowsLowStockUnlessTheFlagWasGiven(t *testing.T) {
	var model, quant, gpu, engine string
	var ctxLen, disk, port int
	var maxPrice, minRel float64
	prof := config.Profile{AllowLowStock: true}

	allow := false
	applyProfile(prof, map[string]bool{}, &model, &quant, &ctxLen, &gpu, &maxPrice, &disk, &minRel, &port, &engine, &allow)
	if !allow {
		t.Error("the profile's low-stock choice was not applied")
	}
	allow = false
	applyProfile(prof, map[string]bool{"allow-low-stock": true}, &model, &quant, &ctxLen, &gpu, &maxPrice, &disk, &minRel, &port, &engine, &allow)
	if allow {
		t.Error("the profile overrode --allow-low-stock=false given on the command line")
	}
}
