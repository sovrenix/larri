// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tmpPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "larri", "config.yaml")
}

// FR-CFG-01: LARRI operates fully with no configuration file, so absence is
// not an error.
func TestMissingFileIsNotAnError(t *testing.T) {
	cfg, found, err := Load(tmpPath(t))
	if err != nil {
		t.Fatalf("a missing config was treated as a failure: %v", err)
	}
	if found {
		t.Error("reported finding a file that does not exist")
	}
	if cfg.Idle.Action != Default().Idle.Action {
		t.Error("did not fall back to the built-in defaults")
	}
}

// A file that exists and cannot be parsed is different: silently falling back
// to defaults would make an operator's ceilings and policies vanish because of
// a stray tab, and the first they would know is the bill.
func TestUnparseableFileIsAnError(t *testing.T) {
	p := tmpPath(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("idle:\n\ttimeout: nonsense\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(p); err == nil {
		t.Fatal("a corrupt config silently became the defaults")
	}
}

// A file that parses but holds an impossible value must not be accepted
// either — it would be acted on.
func TestInvalidValuesAreRejected(t *testing.T) {
	p := tmpPath(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("profiles:\n  default:\n    min_reliability: 4.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(p); err == nil {
		t.Fatal("accepted a reliability floor of 4.2")
	}
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	p := tmpPath(t)
	cfg := Default()
	cfg.Idle.Timeout = 45 * time.Minute
	cfg.Budget.MaxUSD = 5
	cfg.Profiles = map[string]Profile{
		"coder": {Model: "Qwen/Qwen3-Coder-30B", Quantization: "q4_K_M",
			GPUModel: []string{"RTX 4090"}, MaxPriceHr: 0.5, MinReliability: 0.95},
	}
	if err := Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	got, found, err := Load(p)
	if err != nil || !found {
		t.Fatalf("load after save: %v found=%v", err, found)
	}
	if got.Idle.Timeout != 45*time.Minute || got.Budget.MaxUSD != 5 {
		t.Errorf("policy did not survive: %+v", got.Idle)
	}
	pr := got.Profiles["coder"]
	if pr.Model != "Qwen/Qwen3-Coder-30B" || pr.MaxPriceHr != 0.5 || len(pr.GPUModel) != 1 {
		t.Errorf("profile did not survive: %+v", pr)
	}
}

// The file outlives the terminal that created it, so the warning has to live
// in the file. This is the whole reason the format has comments.
func TestWrittenFileCarriesTheWarnings(t *testing.T) {
	p := tmpPath(t)
	if err := Save(p, Default()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.ToLower(string(b))
	for _, want := range []string{"bill by the second", "destroy", "third-party", "keyring"} {
		if !strings.Contains(body, want) {
			t.Errorf("the written file does not mention %q", want)
		}
	}
}

func TestSavedFileIsNotWorldReadable(t *testing.T) {
	p := tmpPath(t)
	if err := Save(p, Default()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("mode = %v; spending limits are nobody else's business", fi.Mode().Perm())
	}
}

// FR-CFG-04, and the hazard behind it: EnsureExists creates and returns. A
// version that prompted would hang every non-interactive caller, the MCP
// server most of all, where a terminal form produces a protocol stream that
// never speaks again.
func TestEnsureExistsCreatesWithoutPrompting(t *testing.T) {
	p := tmpPath(t)
	created, err := EnsureExists(p)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("no file was created")
	}
	again, err := EnsureExists(p)
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Error("overwrote an existing configuration")
	}
}

// An existing file must never be clobbered by the first-run path — that would
// silently discard whatever the operator had set.
func TestEnsureExistsPreservesWhatIsThere(t *testing.T) {
	p := tmpPath(t)
	cfg := Default()
	cfg.Budget.MaxUSD = 12.5
	if err := Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureExists(p); err != nil {
		t.Fatal(err)
	}
	got, _, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Budget.MaxUSD != 12.5 {
		t.Errorf("budget = %v; the operator's setting was overwritten", got.Budget.MaxUSD)
	}
}

// FR-CRIT-05 forbids *silently* reusing criteria for a bare `larri up`. A
// named default profile is allowed to apply, but only because nothing about it
// is silent — the caller is handed the whole profile to echo, not just the
// parts that spend.
func TestResolveReportsTheProfileItApplied(t *testing.T) {
	p := tmpPath(t)
	cfg := Default()
	cfg.Profiles = map[string]Profile{
		DefaultProfile: {Model: "org/m", GPUModel: []string{"RTX 4090"}, MaxPriceHr: 0.5},
	}
	if err := Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	res, err := Resolve(Request{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != DefaultProfile {
		t.Fatalf("profile = %q, want the default one applied", res.Name)
	}
	sum := res.Profile.Summary()
	for _, want := range []string{"org/m", "RTX 4090", "$0.500"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary %q omits %q, so applying it would be silent", sum, want)
		}
	}
}

// The emphasised subset: money and destructive settings taken from a file
// rather than from a flag. Both directions of a stale limit are bad, which is
// why this reports rather than trusts.
func TestMoneyFromAFileIsDisclosed(t *testing.T) {
	p := tmpPath(t)
	cfg := Default()
	cfg.Budget.MaxUSD = 5
	cfg.Profiles = map[string]Profile{DefaultProfile: {MaxPriceHr: 0.25}}
	if err := Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	res, err := Resolve(Request{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range res.Disclose {
		got[d.Setting] = d.Value
	}
	if !strings.Contains(got["max-price"], "0.250") {
		t.Errorf("a price ceiling from the file was not disclosed: %+v", res.Disclose)
	}
	if !strings.Contains(got["budget"], "5.00") {
		t.Errorf("a budget from the file was not disclosed: %+v", res.Disclose)
	}
}

// A flag the operator passed is theirs, and repeating it back as though it
// came from the file would be noise — worse, it would train them to ignore
// the line that matters.
func TestFlagsAreNotDisclosedAsConfig(t *testing.T) {
	p := tmpPath(t)
	cfg := Default()
	cfg.Profiles = map[string]Profile{DefaultProfile: {MaxPriceHr: 0.25}}
	if err := Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	res, err := Resolve(Request{Path: p, SetFlags: map[string]bool{"max-price": true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range res.Disclose {
		if d.Setting == "max-price" {
			t.Error("disclosed a ceiling the operator passed on the command line")
		}
	}
}

// Naming a profile that is not there must fail. Falling back to defaults would
// run against different criteria than the operator believes.
func TestNamingAMissingProfileIsAnError(t *testing.T) {
	p := tmpPath(t)
	if err := Save(p, Default()); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(Request{Path: p, Profile: "nope"}); err == nil {
		t.Fatal("a missing profile was silently ignored")
	}
}

// A missing *default* profile is not an error: it just means no profile layer.
func TestAbsentDefaultProfileIsNotAnError(t *testing.T) {
	p := tmpPath(t)
	if err := Save(p, Default()); err != nil {
		t.Fatal(err)
	}
	res, err := Resolve(Request{Path: p})
	if err != nil {
		t.Fatalf("no default profile should mean no profile layer: %v", err)
	}
	if res.Name != "" {
		t.Errorf("applied profile %q that does not exist", res.Name)
	}
}
