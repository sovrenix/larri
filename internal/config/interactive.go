// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"os"

	"golang.org/x/term"
)

// Mode is how a LARRI process may talk to whoever invoked it.
type Mode int

const (
	// ModeInteractive: a human is at a terminal and may be prompted.
	ModeInteractive Mode = iota
	// ModeNonInteractive: proceed on defaults, report what was assumed.
	ModeNonInteractive
)

func (m Mode) String() string {
	if m == ModeInteractive {
		return "interactive"
	}
	return "non-interactive"
}

// Interactive reports whether prompting is permitted.
func (m Mode) Interactive() bool { return m == ModeInteractive }

// Invocation describes how the process was started. It exists so that
// interactivity is *detected* rather than assumed (FR-CFG-02).
type Invocation struct {
	// Daemon, MCP, and JSON are surfaces that must never prompt.
	//
	// MCP is the one that would actually bite: larri_up is an MCP tool, so an
	// agent can reach a first run. A prompt on that path is not a prompt, it
	// is a hang with no output and nothing able to answer it.
	Daemon bool
	MCP    bool
	JSON   bool

	// ForceNonInteractive is --non-interactive or --yes.
	ForceNonInteractive bool
}

// DetectMode decides whether prompting is allowed.
//
// The default is non-interactive. Every condition must affirmatively hold for
// a prompt to be permitted, because the cost of wrongly prompting (a hung
// agent, a stalled CI job, a daemon that never starts) is much higher than the
// cost of wrongly proceeding on documented defaults.
func DetectMode(inv Invocation, env func(string) string) Mode {
	if inv.Daemon || inv.MCP || inv.JSON || inv.ForceNonInteractive {
		return ModeNonInteractive
	}
	// Honour the conventional environment switches.
	for _, k := range []string{"LARRI_NON_INTERACTIVE", "CI"} {
		if v := env(k); v != "" && v != "0" && v != "false" {
			return ModeNonInteractive
		}
	}
	// A terminal on both stdin and stderr. stderr rather than stdout, because
	// `larri offers | jq` is a normal thing to do and must still be able to
	// prompt about something unrelated on the error stream.
	if !isTerminal(os.Stdin) || !isTerminal(os.Stderr) {
		return ModeNonInteractive
	}
	return ModeInteractive
}

func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }
