// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
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
