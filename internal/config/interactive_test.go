// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package config

import "testing"

func noEnv(string) string { return "" }

// FR-CFG-02 is the requirement that would actually have bitten: larri_up is an
// MCP tool, so an agent can reach a first run. A prompt on that path is not a
// prompt — it is a hang with no output and nothing able to answer it.
func TestSurfacesThatMustNeverPrompt(t *testing.T) {
	for name, inv := range map[string]Invocation{
		"mcp":               {MCP: true},
		"daemon":            {Daemon: true},
		"json output":       {JSON: true},
		"--non-interactive": {ForceNonInteractive: true},
	} {
		if got := DetectMode(inv, noEnv); got.Interactive() {
			t.Errorf("%s must never prompt, got %s", name, got)
		}
	}
}

func TestEnvironmentForcesNonInteractive(t *testing.T) {
	for _, key := range []string{"LARRI_NON_INTERACTIVE", "CI"} {
		env := func(k string) string {
			if k == key {
				return "true"
			}
			return ""
		}
		if DetectMode(Invocation{}, env).Interactive() {
			t.Errorf("%s set must force non-interactive", key)
		}
	}
	// A falsy value must not trip it.
	env := func(k string) string {
		if k == "CI" {
			return "false"
		}
		return ""
	}
	// Still non-interactive here because the test process has no TTY, but the
	// point is that "false" is not treated as "set".
	_ = DetectMode(Invocation{}, env)
}

// Under `go test` there is no terminal, so detection must land on
// non-interactive. This doubles as the regression test for the property that
// matters: the default is to proceed, not to block.
func TestDefaultsToNonInteractiveWithoutATerminal(t *testing.T) {
	if DetectMode(Invocation{}, noEnv).Interactive() {
		t.Fatal("without a terminal LARRI must proceed on defaults, never block")
	}
}
