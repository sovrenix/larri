// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/provider"
	pfake "go.sovrenix.com/larri/internal/provider/fake"
	"go.sovrenix.com/larri/internal/state"
)

func noop(context.Context, json.RawMessage) (any, error) { return nil, nil }

// Two definitions under one name is the divergence this package exists to
// prevent, and silently keeping the last registered would make which one wins
// depend on file order.
func TestDuplicateNamesAreRefused(t *testing.T) {
	r := NewRegistry()
	if err := r.Add(Tool{Name: "larri_down", Handler: noop}); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(Tool{Name: "larri_down", Handler: noop}); err == nil {
		t.Fatal("a second larri_down was accepted")
	}
}

func TestHandlerlessToolIsRefused(t *testing.T) {
	if err := NewRegistry().Add(Tool{Name: "larri_x"}); err == nil {
		t.Fatal("a tool with no handler was accepted")
	}
}

// The exposure split is the point: the read-only set is identical for both
// drivers, and the set that spends money is not offered to the model running
// on the rig — a model that can rent hardware in response to its own output is
// a spending loop with no human in it.
func TestConsequentialToolsAreNotExposedToBothDrivers(t *testing.T) {
	r := NewRegistry()
	r.MustAdd(Tool{Name: "larri_status", Handler: noop})
	r.MustAdd(Tool{Name: "larri_up", Handler: noop, Consequential: true, Exposure: ExposeMCPOnly})

	for _, tool := range r.For(ExposeBoth) {
		if tool.Consequential {
			t.Errorf("%s spends money and is exposed to both drivers", tool.Name)
		}
	}
	if len(r.For(ExposeMCPOnly)) != 2 {
		t.Error("the mcp driver should see every tool")
	}
}

// Every tool that spends or destroys must say so in its description, because
// the driving agent reports that text to a person before calling it.
func TestEveryConsequentialToolStatesTheCost(t *testing.T) {
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	r := NewRegistry()
	if err := Register(r, Deps{
		Store:           st,
		NewOrchestrator: func(string, string, core.ModelSpec) (*daemon.Orchestrator, error) { return nil, nil },
	}); err != nil {
		t.Fatal(err)
	}
	for _, tool := range r.All() {
		if !tool.Consequential {
			continue
		}
		d := strings.ToUpper(tool.Description)
		if !strings.Contains(d, "SPENDS MONEY") && !strings.Contains(d, "DESTROYS") {
			t.Errorf("%s spends or destroys but does not say so: %q", tool.Name, tool.Description)
		}
		if tool.Exposure != ExposeMCPOnly {
			t.Errorf("%s is consequential but exposed to the chat pane by default", tool.Name)
		}
	}
}

// The read-only tools are the ones an agent should feel free to call, so they
// must not be marked consequential by accident.
func TestReadOnlyToolsAreNotConsequential(t *testing.T) {
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := NewRegistry()
	if err := Register(r, Deps{Store: st,
		NewOrchestrator: func(string, string, core.ModelSpec) (*daemon.Orchestrator, error) { return nil, nil }}); err != nil {
		t.Fatal(err)
	}
	readOnly := map[string]bool{
		"larri_status": true, "larri_plan": true, "larri_search_offers": true,
		"larri_logs": true, "larri_orphans": true,
	}
	for _, tool := range r.All() {
		if readOnly[tool.Name] && tool.Consequential {
			t.Errorf("%s is read-only but marked consequential", tool.Name)
		}
	}
}

// The guard that keeps an agent from renting a second rig alongside a billing
// one. An agent is likelier to do this than a person, because it cannot see
// the terminal it did not print to.
func TestUpRefusesWhileARigIsBilling(t *testing.T) {
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	rig := newRig(t, st)
	d := Deps{Store: st, NewOrchestrator: func(string, string, core.ModelSpec) (*daemon.Orchestrator, error) {
		t.Fatal("an orchestrator was built before the billing check")
		return nil, nil
	}}
	_, err = d.up(context.Background(), json.RawMessage(`{"model":"org/model"}`))
	if err == nil {
		t.Fatal("a second rig was rented alongside a billing one")
	}
	if !strings.Contains(err.Error(), rig.ID) {
		t.Errorf("error should name the billing rig, got: %v", err)
	}
}

func newRig(t *testing.T, st *state.Store) *core.Rig {
	t.Helper()
	id, err := state.NewID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rig := &core.Rig{ID: id, Offer: core.Offer{PriceHr: 0.44}}
	if err := st.Transition(rig, core.StateReady, "test"); err != nil {
		t.Fatal(err)
	}
	return rig
}

// larri_down tears a rig down through the provider holding it. The MCP server
// opened the configured default for every call, and a provider that never
// held an instance answers "not found" — which teardown reads as confirmed
// absence of a machine still billing on the other one.
func TestDownAsksForTheRigsOwnProvider(t *testing.T) {
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rig := newRig(t, st)
	rig.Offer.Provider = "runpod"
	if err := st.Save(rig); err != nil {
		t.Fatal(err)
	}
	var asked string
	d := Deps{Store: st, NewOrchestrator: func(_, prov string, _ core.ModelSpec) (*daemon.Orchestrator, error) {
		asked = prov
		return nil, errors.New("stop here")
	}}
	_, _ = d.down(context.Background(), json.RawMessage(`{"rig":"`+rig.ID+`"}`))
	if asked != "runpod" {
		t.Errorf("orchestrator built for provider %q; the rig is on runpod", asked)
	}
}

// The engine is chosen from the model when the agent names none, as the CLI
// chooses it. The factory used to be handed nothing, so a named .gguf file or
// a Q4_K_M request was planned and rented on vLLM, which loads neither.
func TestTheEngineIsChosenFromTheModelTheAgentNamed(t *testing.T) {
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var got core.ModelSpec
	d := Deps{Store: st, NewOrchestrator: func(_, _ string, model core.ModelSpec) (*daemon.Orchestrator, error) {
		got = model
		return nil, errors.New("stop here")
	}}
	raw := json.RawMessage(`{"model":"unsloth/Qwen3-8B-GGUF/Qwen3-8B-Q8_0.gguf","quantization":"Q8_0"}`)
	for name, call := range map[string]func(context.Context, json.RawMessage) (any, error){
		"larri_plan": d.plan, "larri_search_offers": d.searchOffers,
	} {
		got = core.ModelSpec{}
		_, _ = call(context.Background(), raw)
		if got.Ref != "unsloth/Qwen3-8B-GGUF/Qwen3-8B-Q8_0.gguf" || got.Quantization != "Q8_0" {
			t.Errorf("%s built its orchestrator from %+v, not the model asked for", name, got)
		}
	}
}

// An orphan is something LARRI lost track of, so the provider holding it is
// not known in advance. Listing only the default hid a RunPod pod on a machine
// configured for Vast; destroying by id alone sent it to the wrong provider.
func TestOrphansAreFoundAndDestroyedAtTheProviderHoldingThem(t *testing.T) {
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	held := pfake.New("runpod", nil, pfake.Behaviour{})
	lost, err := held.Create(context.Background(), core.Offer{OfferID: "o", PriceHr: 0.5},
		provider.CreateSpec{Label: core.LabelKey + ":lost-rig"})
	if err != nil {
		t.Fatal(err)
	}
	fakes := map[string]*pfake.Provider{
		"vastai": pfake.New("vastai", nil, pfake.Behaviour{Unreachable: true}),
		"runpod": held,
	}
	var built []string
	d := Deps{
		Store:     st,
		Providers: func() []string { return []string{"vastai", "runpod"} },
		NewOrchestrator: func(_, prov string, _ core.ModelSpec) (*daemon.Orchestrator, error) {
			built = append(built, prov)
			return &daemon.Orchestrator{Store: st, Provider: fakes[prov]}, nil
		},
	}
	out, err := d.orphans(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	res := out.(map[string]any)
	rows := res["orphans"].([]map[string]any)
	if len(rows) != 1 || rows[0]["provider"] != "runpod" || rows[0]["instance_id"] != lost.InstanceID {
		t.Errorf("orphans = %v; the pod on the second provider must be listed with its provider", rows)
	}
	if _, ok := res["not_checked"].(map[string]string)["vastai"]; !ok {
		t.Errorf("not_checked = %v; an unreachable provider is reported, not read as empty", res["not_checked"])
	}

	if _, err := d.orphanDestroy(context.Background(), json.RawMessage(`{"instance_id":"`+lost.InstanceID+`"}`)); err == nil {
		t.Error("destroyed an orphan without being told which provider holds it")
	}
	built = nil
	if _, err := d.orphanDestroy(context.Background(),
		json.RawMessage(`{"instance_id":"`+lost.InstanceID+`","provider":"runpod"}`)); err != nil {
		t.Fatal(err)
	}
	if len(built) != 1 || built[0] != "runpod" || held.Count() != 0 {
		t.Errorf("destroy went through %v; want runpod, and the pod gone", built)
	}
}
