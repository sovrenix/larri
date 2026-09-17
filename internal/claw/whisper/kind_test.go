// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package whisper

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/claw"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/wire"
)

// The real large-v3 repository, near enough for the arithmetic to be real.
const largeV3Bytes = 3_090_000_000

func sizes() StaticMeasurer {
	return StaticMeasurer{
		"Systran/faster-whisper-large-v3": largeV3Bytes,
		"Systran/faster-whisper-tiny":     75_000_000,
	}
}

func planJob(t *testing.T, job string, opt claw.Options) (*Kind, *claw.Plan) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "job.yml")
	if err := os.WriteFile(path, []byte(job), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := claw.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	k := &Kind{measurer: sizes()}
	p, err := k.Plan(context.Background(), cfg, opt)
	if err != nil {
		t.Fatal(err)
	}
	return k, p
}

// The registry is the only way the daemon reaches this package, and the site
// is what decides that teardown reverts wiring rather than collecting files.
func TestWhisperIsRegisteredAsALocalClaw(t *testing.T) {
	k, err := claw.Open(Type)
	if err != nil {
		t.Fatal(err)
	}
	if k.Site() != claw.SiteLocal {
		t.Errorf("site = %q, want local: the app stays on the operator's machine", k.Site())
	}
	if _, ok := k.(claw.Local); !ok {
		t.Fatal("a local claw that names no clients would rent inference nothing can reach")
	}
	if _, ok := k.(claw.Remote); ok {
		t.Error("a local claw offered to collect outputs from a host that produces none")
	}
}

func TestThePlanIsDerivedFromTheMeasuredRepository(t *testing.T) {
	k, p := planJob(t, "type: whisper\nmodel: large-v3\n", claw.Options{})

	if p.ColdStartBytes != largeV3Bytes {
		t.Errorf("ColdStartBytes = %d, want the measured repository", p.ColdStartBytes)
	}
	if p.Sizing == nil {
		t.Fatal("Sizing is nil: whisper's window is fixed, so the transformer path would size it wrong")
	}
	if p.Model.Ref != "Systran/faster-whisper-large-v3" {
		t.Errorf("Model.Ref = %q, the catalogue name was not resolved", p.Model.Ref)
	}
	if p.Criteria.VRAMPerGPUGB != p.Criteria.VRAMTotalGB {
		t.Errorf("per-gpu %d, total %d: a server that pins one device cannot use a sum",
			p.Criteria.VRAMPerGPUGB, p.Criteria.VRAMTotalGB)
	}
	if p.Criteria.VRAMPerGPUGB < 6 || p.Criteria.VRAMPerGPUGB > 10 {
		t.Errorf("vram floor = %d GB, which is not a card large-v3 wants", p.Criteria.VRAMPerGPUGB)
	}
	if k.resident != k.downloaded {
		t.Error("float16 should be resident at its downloaded size")
	}
}

// CTranslate2 quantises at load rather than at publication, so the download
// does not shrink and the resident weights do. Conflating them sizes the disk
// or the card wrong, in opposite directions.
func TestQuantisingShrinksTheCardAndNotTheDownload(t *testing.T) {
	_, fp16 := planJob(t, "type: whisper\nmodel: large-v3\n", claw.Options{})
	k, int8 := planJob(t,
		"type: whisper\nmodel: large-v3\ncompute_type: int8_float16\n", claw.Options{})

	if int8.ColdStartBytes != fp16.ColdStartBytes {
		t.Errorf("the download shrank with quantisation: %d vs %d",
			int8.ColdStartBytes, fp16.ColdStartBytes)
	}
	if int8.Criteria.DiskGB != fp16.Criteria.DiskGB {
		t.Errorf("the disk floor moved with quantisation: %d vs %d",
			int8.Criteria.DiskGB, fp16.Criteria.DiskGB)
	}
	if int8.Sizing.RequiredVRAMBytes >= fp16.Sizing.RequiredVRAMBytes {
		t.Error("quantising bought no vram at all")
	}
	if k.resident >= k.downloaded {
		t.Errorf("resident %d is not below downloaded %d", k.resident, k.downloaded)
	}
	if !containsIn(int8.Summary, "resident") {
		t.Errorf("the summary does not explain the two figures: %q", int8.Summary)
	}
}

// Renting something smaller than what was asked for is the one direction that
// cannot be undone after the fact (FR-CLAW-04).
func TestThePlanOnlyRaisesTheOperatorsFloors(t *testing.T) {
	base := core.Criteria{VRAMPerGPUGB: 48, VRAMTotalGB: 48, RAMGB: 128, DiskGB: 400}
	_, p := planJob(t, "type: whisper\nmodel: tiny\n", claw.Options{Criteria: base})

	if p.Criteria.VRAMPerGPUGB != 48 || p.Criteria.VRAMTotalGB != 48 {
		t.Errorf("vram lowered to %d/%d from 48/48",
			p.Criteria.VRAMPerGPUGB, p.Criteria.VRAMTotalGB)
	}
	if p.Criteria.RAMGB != 128 || p.Criteria.DiskGB != 400 {
		t.Errorf("ram/disk lowered to %d/%d", p.Criteria.RAMGB, p.Criteria.DiskGB)
	}
}

// A name the catalogue has never heard of must still work, or every new
// conversion waits on an edit to this package.
func TestAFullRepositoryBypassesTheCatalogue(t *testing.T) {
	if got := Repo("someone/their-own-conversion"); got != "someone/their-own-conversion" {
		t.Errorf("Repo rewrote a full repository to %q", got)
	}
	if got := Repo("large-v3"); got != "Systran/faster-whisper-large-v3" {
		t.Errorf("the catalogue did not resolve large-v3: %q", got)
	}
	if got := Repo(""); got != DefaultModel {
		t.Errorf("an unset model did not default: %q", got)
	}
}

// Each named client gets its own writer, which is what buys a revocable
// credential per client and an identity the probe can attribute.
func TestEachNamedClientBecomesItsOwnWriter(t *testing.T) {
	k, _ := planJob(t,
		"type: whisper\nmodel: tiny\nclients: [subtitle-edit, buzz]\n", claw.Options{})

	ws := k.Clients()
	if len(ws) != 2 {
		t.Fatalf("built %d writers for two clients", len(ws))
	}
	if got := wire.Names(ws); got[0] != "buzz" || got[1] != "subtitle-edit" {
		t.Errorf("names = %v", got)
	}
	for _, w := range ws {
		if w.Tier().Writable() {
			t.Errorf("%s claims it can write config for an app nobody has observed", w.Name())
		}
		if len(wire.InstructionsFor(w, wire.Endpoint{URL: "http://127.0.0.1:8188/v1"})) == 0 {
			t.Errorf("%s is guided and says nothing to paste", w.Name())
		}
	}
}

// A job that names no client still wires one, because a rig nothing is pointed
// at is a rig nobody can use.
func TestAJobWithNoClientsStillWiresOne(t *testing.T) {
	k, p := planJob(t, "type: whisper\nmodel: tiny\n", claw.Options{})
	if len(k.Clients()) != 1 {
		t.Errorf("wired %d clients by default", len(k.Clients()))
	}
	if !containsIn(p.Summary, "wiring") {
		t.Errorf("the summary does not say what will be wired: %q", p.Summary)
	}
}

// The guided tier is a fact the operator should be told before spending, not
// one they infer from a letter in a record afterwards.
func TestTheGuidedTierIsDisclosedUpFront(t *testing.T) {
	_, p := planJob(t, "type: whisper\nmodel: tiny\n", claw.Options{})
	if !containsIn(p.Caveats, "guided") {
		t.Errorf("caveats do not mention that wiring is guided: %q", p.Caveats)
	}
	if !containsIn(p.Caveats, "visible to the host") {
		t.Errorf("caveats do not disclose what the host can read: %q", p.Caveats)
	}
}

func TestTheServerCarriesTheJobsSettings(t *testing.T) {
	k, p := planJob(t, "type: whisper\nmodel: large-v3\n"+
		"compute_type: int8_float16\nlanguage: en\nimage: example/whisper:pinned\n",
		claw.Options{})

	r, ok := k.Server(p).(*Runtime)
	if !ok {
		t.Fatal("Server did not build a whisper runtime")
	}
	if r.Model != "Systran/faster-whisper-large-v3" {
		t.Errorf("model = %q", r.Model)
	}
	if r.ComputeType != "int8_float16" || r.Language != "en" {
		t.Errorf("settings dropped: %q / %q", r.ComputeType, r.Language)
	}
	if r.ImageRef != "example/whisper:pinned" {
		t.Errorf("image = %q", r.ImageRef)
	}
	if r.ModelBytes != largeV3Bytes {
		t.Error("the server was not told how large the download is, so progress has no total")
	}
}

// A model that does not exist, or one the token cannot read, must end the run
// locally rather than on a rented host.
func TestAnUnmeasurableModelEndsTheRunBeforeAnythingIsRented(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job.yml")
	if err := os.WriteFile(path, []byte("type: whisper\nmodel: nobody/nothing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := claw.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	k := &Kind{measurer: sizes()}
	if _, err := k.Plan(context.Background(), cfg, claw.Options{}); err == nil {
		t.Fatal("a model with no measurable size planned successfully")
	}
}

func containsIn(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}
