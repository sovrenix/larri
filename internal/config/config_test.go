// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"strings"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
)

// FR-CFG-01: a machine that has never been configured must work. The defaults
// are therefore not a placeholder — they are the shipping configuration.
func TestDefaultsAreUsable(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("built-in defaults must be valid, got %v", err)
	}
	if c.MaxConcurrentRigs != 1 {
		t.Errorf("Q-05: one rig at a time, got %d", c.MaxConcurrentRigs)
	}
	if c.Idle.Action != IdleDestroy || c.Idle.Timeout != 30*time.Minute {
		t.Errorf("Q-11: idle defaults to 30m→destroy, got %v→%s", c.Idle.Timeout, c.Idle.Action)
	}
	if c.Budget.Action != IdleDestroy {
		t.Errorf("Q-03: budget breach defaults to destroy, got %s", c.Budget.Action)
	}
	if len(c.Clients) != 0 {
		t.Error("clients must come from detection, never from a guess (FR-WIRE protocol step 1)")
	}
	w := c.RankWeights
	if sum := w.Price + w.Fit + w.Reliability + w.Net + w.Region; sum < 0.999 || sum > 1.001 {
		t.Errorf("rank weights should sum to 1.0, got %.3f", sum)
	}
}

// FR-CFG-03: the defaults that destroy things get stated whether or not
// anyone is at a terminal to read them.
func TestDestructiveDefaultsAreDisclosed(t *testing.T) {
	ds := Default().Disclosures()
	var idle *Disclosure
	for i := range ds {
		if ds[i].Setting == "idle-timeout" {
			idle = &ds[i]
		}
	}
	if idle == nil {
		t.Fatal("idle reclamation must always be disclosed: it destroys rigs")
	}
	if !idle.Destructive {
		t.Error("a 30m→destroy default must be flagged destructive")
	}
	if idle.ChangeWith == "" {
		t.Error("a disclosure without the command to change it is just an announcement")
	}
	if !strings.Contains(idle.Value, "destroy") {
		t.Errorf("the disclosure must name the action, got %q", idle.Value)
	}
}

func TestValidateRejectsUnworkableConfig(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"port out of range", func(c *Config) { c.LocalPort = 0 }},
		{"zero rigs", func(c *Config) { c.MaxConcurrentRigs = 0 }},
		{"unknown idle action", func(c *Config) { c.Idle.Action = "explode" }},
		{"negative budget", func(c *Config) { c.Budget.MaxUSD = -1 }},
		{"no providers", func(c *Config) { c.Providers = nil }},
	}
	for _, tc := range cases {
		c := Default()
		tc.mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", tc.name)
		}
	}
}

// FR-CRIT-05: profiles are named and explicit. There is no "last used" slot,
// because a bare `larri up` that silently reapplies a fortnight-old criteria
// set can buy hardware nobody intended.
func TestProfilesAreNamedNotImplicit(t *testing.T) {
	c := Default()
	if c.Profiles != nil {
		t.Error("no profiles exist until one is saved by name")
	}
	c.Profiles = map[string]Profile{
		"coder": {GPUModel: []string{"A100"}, MaxPriceHr: 1.50},
	}
	if _, ok := c.Profiles["coder"]; !ok {
		t.Fatal("a saved profile is retrievable by its name")
	}
	if _, ok := c.Profiles["last"]; ok {
		t.Error("there must be no implicit last-used profile")
	}
}

// Q-04 reaches the defaults through core.Criteria's zero value: an unset
// Interruptible forbids. A profile saved without thinking about it inherits
// the safe posture.
func TestZeroCriteriaForbidsInterruptible(t *testing.T) {
	var c core.Criteria
	if c.Interruptible != core.Forbid {
		t.Fatalf("zero criteria must forbid interruptible offers, got %v", c.Interruptible)
	}
}
