// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package clients

import (
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/wire"
)

func endpoint() wire.Endpoint {
	return wire.Endpoint{
		URL:   "http://127.0.0.1:8188/v1",
		Model: "whisper",
		Key:   secret.New("sk-larri-abc123"),
	}
}

// The tier is a claim about how an application reloads, and this writer has
// observed no application at all. Claiming TierFile would promise byte-exact
// revert of a file it has never seen.
func TestTheGuidedWriterDoesNotClaimItCanWriteFiles(t *testing.T) {
	w := NewOpenAIAudio("subtitle-edit")
	if w.Tier() != wire.TierGuided {
		t.Errorf("tier = %q, want C", w.Tier())
	}
	if w.Tier().Writable() {
		t.Error("a guided writer reported itself writable, so Apply would edit somebody's config")
	}
}

// Everything an operator has to paste, and nothing that would have to be
// pasted again after a teardown (FR-WIRE-06).
func TestTheInstructionsCarryTheStableEndpointAndNothingElse(t *testing.T) {
	lines := NewOpenAIAudio("buzz").Instructions(endpoint())
	joined := strings.Join(lines, "\n")

	for _, want := range []string{"http://127.0.0.1:8188/v1", "whisper", "sk-larri-abc123", "buzz"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the instructions omit %q:\n%s", want, joined)
		}
	}
	// The ephemeral host is the thing that must never reach a client's
	// configuration, and wire.Endpoint has no field to carry it — so the only
	// way one could appear here is if somebody added one.
	for _, forbidden := range []string{"runpod", "vast", "proxy.runpod.net", "ssh"} {
		if strings.Contains(strings.ToLower(joined), forbidden) {
			t.Errorf("the instructions name the provider (%q), which changes every teardown:\n%s",
				forbidden, joined)
		}
	}
}

// A key is mandatory on the local endpoint (FR-SEC-09), but an endpoint
// without one must not print an empty line where a credential should be.
func TestInstructionsOmitWhatTheEndpointDoesNotHave(t *testing.T) {
	lines := NewOpenAIAudio("x").Instructions(wire.Endpoint{URL: "http://127.0.0.1:8188/v1"})
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "api key") {
		t.Errorf("an endpoint with no key advertised one:\n%s", joined)
	}
	if strings.Contains(joined, "model") {
		t.Errorf("an endpoint with no served name advertised one:\n%s", joined)
	}
}

// wire.Apply must not call Apply for an unwritable tier — it builds the record
// itself. The runner is what enforces that; this pins the writer's side of it.
func TestApplyWritesNothingAndRevertUndoesNothing(t *testing.T) {
	w := NewOpenAIAudio("x")
	rec, err := w.Apply(endpoint())
	if err != nil {
		t.Fatal(err)
	}
	if rec.Path != "" || rec.BackupPath != "" {
		t.Errorf("a guided writer recorded a file it did not touch: %+v", rec)
	}
	if err := w.Revert(rec); err != nil {
		t.Errorf("reverting nothing failed: %v", err)
	}
}

// The runner is what actually drives this in production, so the whole path is
// worth one pass: detected, recorded at the right tier, and verified by the
// probe rather than by anything on disk.
func TestTheRunnerRecordsAndVerifiesAGuidedClient(t *testing.T) {
	w := NewOpenAIAudio("subtitle-edit")

	var asked string
	probe := func(client string) (bool, error) {
		asked = client
		return true, nil
	}
	recs, errList := wire.Apply([]wire.ClientWriter{w}, endpoint(), probe)
	if len(errList) != 0 {
		t.Fatalf("apply reported errors: %v", errList)
	}
	if len(recs) != 1 {
		t.Fatalf("recorded %d clients, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Client != "subtitle-edit" || rec.Tier != string(wire.TierGuided) {
		t.Errorf("record = %+v", rec)
	}
	if rec.Path != "" {
		t.Errorf("a guided client recorded a path: %q", rec.Path)
	}
	if !rec.Verified {
		t.Error("the probe said the client reached the endpoint and the record does not")
	}
	if asked != "subtitle-edit" {
		t.Errorf("the probe was asked about %q", asked)
	}

	// Nothing was written, so teardown has nothing to undo — and must not
	// report an error for the absence.
	if errs := wire.Revert(recs, wire.Index([]wire.ClientWriter{w})); len(errs) != 0 {
		t.Errorf("reverting a guided client produced errors: %v", errs)
	}
}

// Without a probe there is no evidence at all, and the record has to say so
// rather than claim success. For a guided client this is the only signal
// there is: nothing was written, so there is no file to inspect instead.
func TestAnUnprobedGuidedClientIsNotClaimedVerified(t *testing.T) {
	recs, _ := wire.Apply([]wire.ClientWriter{NewOpenAIAudio("x")}, endpoint(), nil)
	if len(recs) != 1 {
		t.Fatalf("recorded %d clients, want 1", len(recs))
	}
	if recs[0].Verified {
		t.Error("a client nobody probed was recorded as verified")
	}
}
