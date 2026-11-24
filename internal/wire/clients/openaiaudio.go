// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package clients holds the client writers that configure local applications
// against a rig's endpoint (§10.2).
//
// One package rather than one per application, because a writer is a small
// thing — detect, back up, write, record, revert — and the interesting content
// is the *tier*, which is a claim about how a particular application reloads
// rather than about how LARRI writes files.
package clients

import (
	"fmt"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/wire"
)

// OpenAIAudio is the guided writer for any application that can be pointed at
// an OpenAI-compatible audio endpoint.
//
// Generic on purpose, and it is the first writer for that reason. A writer per
// application bets the integration on that application's configuration format
// — whether Subtitle Edit's Settings.xml survives being edited while it runs,
// whether Buzz reads QSettings once or watches it — and every one of those bets
// has to be settled by observing the application rather than reading its docs
// (FR-WIRE-13). None of them need settling before the endpoint is useful: the
// operator pastes two values into whatever they already use, and the probe says
// whether it worked.
//
// The expectation is that most transcription front-ends land here permanently.
// They are desktop GUI applications that read their settings at startup and
// write them back on exit, which is tier B behaviour whatever the file format
// looks like — an edit made while the application is running is clobbered when
// the operator closes it.
type OpenAIAudio struct {
	// App is what to call this client, and keys both the WiringRecord and the
	// per-client token the probe attributes requests to.
	App string
}

var (
	_ wire.ClientWriter = (*OpenAIAudio)(nil)
	_ wire.Guided       = (*OpenAIAudio)(nil)
)

// NewOpenAIAudio builds the writer for a named application.
func NewOpenAIAudio(app string) *OpenAIAudio {
	if app == "" {
		app = "openai-audio"
	}
	return &OpenAIAudio{App: app}
}

func (o *OpenAIAudio) Name() string { return o.App }

// Tier is guided, which is a decision rather than a fallback.
//
// LARRI could write a config file for a named application and does not, because
// the tier is decided by observed reload behaviour and no such observation has
// been made. Claiming TierFile without it would promise byte-exact revert of a
// file this code has never seen.
func (o *OpenAIAudio) Tier() wire.Tier { return wire.TierGuided }

// Detect reports true, and the operator naming this client is the detection.
//
// There is nothing on disk to look for: this writer configures no particular
// application, so "installed here" has no file to answer it. The rule Detect
// exists to enforce — never write config for a client that is not present — is
// satisfied trivially, since nothing is written at all.
func (o *OpenAIAudio) Detect() (bool, error) { return true, nil }

// Apply is never called: wire.Apply builds the record itself for a tier that
// is not writable. Implemented honestly rather than left to panic, so a direct
// caller gets a record that correctly says nothing was written.
func (o *OpenAIAudio) Apply(wire.Endpoint) (core.WiringRecord, error) {
	return core.WiringRecord{Client: o.App, Tier: string(wire.TierGuided)}, nil
}

// Revert is a no-op, which is the honest answer: nothing was changed, so there
// is nothing to put back. The operator's own configuration outlives the rig and
// will point at a local port that stops answering — which is why the endpoint
// they were given is the stable one (invariant 3) and never the provider's.
func (o *OpenAIAudio) Revert(core.WiringRecord) error { return nil }

// Instructions are what to paste, and where.
//
// Three values and no prose about which menu they live in, because that varies
// per application and a wrong instruction is worse than none. The API key is
// included because the local endpoint requires one (FR-SEC-09): loopback is not
// a per-user boundary, and for LARRI a request that fires is a request that
// spends.
func (o *OpenAIAudio) Instructions(ep wire.Endpoint) []string {
	out := []string{
		fmt.Sprintf("point %s at this endpoint, once:", o.App),
		"  base url   " + ep.URL,
	}
	if ep.Model != "" {
		out = append(out, "  model      "+ep.Model)
	}
	if !ep.Key.Empty() {
		out = append(out, "  api key    "+ep.Key.Reveal())
	}
	out = append(out,
		"the address is stable across rig replacement, so this is configured once and not again")
	return out
}
