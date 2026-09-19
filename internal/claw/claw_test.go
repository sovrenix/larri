// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package claw

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/runtime"
)

// stub is the smallest thing that satisfies Kind, for registry tests.
type stub struct {
	typ  Type
	site Site
}

func (s stub) Type() Type       { return s.typ }
func (s stub) Site() Site       { return s.site }
func (s stub) Describe() string { return "a stub" }
func (s stub) Plan(context.Context, *Config, Options) (*Plan, error) {
	return &Plan{}, nil
}
func (s stub) Server(*Plan) runtime.Workload { return nil }

var _ Kind = stub{}

// A Kind carries state from Plan through to Server, so Open must hand back a
// fresh one. A shared instance would let two claws overwrite each other's plan,
// and the failure would be a rig serving the wrong thing rather than an error.
func TestOpenReturnsAFreshInstance(t *testing.T) {
	t.Cleanup(clearRegistry)
	var built int
	Register("counted", func() Kind {
		built++
		return stub{typ: "counted", site: SiteRemote}
	})

	if _, err := Open("counted"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open("counted"); err != nil {
		t.Fatal(err)
	}
	if built != 2 {
		t.Errorf("factory ran %d times for two Opens; the registry is handing out a singleton", built)
	}
}

// Two Kinds answering to one name would make which is used depend on import
// order, and the wrong one spends money.
func TestDuplicateRegistrationPanics(t *testing.T) {
	t.Cleanup(clearRegistry)
	Register("dup", func() Kind { return stub{typ: "dup", site: SiteRemote} })

	defer func() {
		if recover() == nil {
			t.Error("registering a type twice was allowed")
		}
	}()
	Register("dup", func() Kind { return stub{typ: "dup", site: SiteLocal} })
}

// A Kind registered under one name and answering to another would make --type
// mean one thing to the registry and another to everything downstream.
func TestOpenRefusesAKindThatDisagreesWithItsName(t *testing.T) {
	t.Cleanup(clearRegistry)
	Register("declared", func() Kind { return stub{typ: "actual", site: SiteRemote} })

	if _, err := Open("declared"); err == nil {
		t.Fatal("a Kind reporting the wrong type was accepted")
	}
}

// Site decides what the lifecycle does either side of the rental, so a Kind
// that reports neither is not usable at all.
func TestOpenRefusesAnInvalidSite(t *testing.T) {
	t.Cleanup(clearRegistry)
	Register("siteless", func() Kind { return stub{typ: "siteless", site: Site("nowhere")} })

	if _, err := Open("siteless"); err == nil {
		t.Fatal("a Kind with no valid site was accepted")
	}
}

// The error an operator sees for a typo has to name what is available, and say
// something useful when nothing is.
func TestUnknownTypeNamesWhatIsKnown(t *testing.T) {
	t.Cleanup(clearRegistry)

	_, err := Open("nope")
	if err == nil || !strings.Contains(err.Error(), "none are compiled in") {
		t.Errorf("with an empty registry: %v", err)
	}

	Register("real", func() Kind { return stub{typ: "real", site: SiteLocal} })
	_, err = Open("nope")
	if err == nil || !strings.Contains(err.Error(), "real") {
		t.Errorf("error does not name the known types: %v", err)
	}
}

// --list has to say the site, because it is the fact that changes what the
// operator gets: a browser session whose results come back at teardown, or an
// application they already run being pointed somewhere.
func TestDescribeNamesTheSite(t *testing.T) {
	t.Cleanup(clearRegistry)
	Register("rem", func() Kind { return stub{typ: "rem", site: SiteRemote} })
	Register("loc", func() Kind { return stub{typ: "loc", site: SiteLocal} })

	lines := strings.Join(Describe(), "\n")
	for _, want := range []string{"rem", "remote", "loc", "local", "a stub"} {
		if !strings.Contains(lines, want) {
			t.Errorf("--list output missing %q:\n%s", want, lines)
		}
	}
}

// ---- criteria ---------------------------------------------------------

// An operator who asked for 80 GB has said something about the hardware they
// want. A claw needing 12 GB has not contradicted them, and quietly lowering
// the floor would rent something cheaper than what was asked for — the one
// direction that cannot be undone after the fact.
func TestRaiseCriteriaNeverLowers(t *testing.T) {
	base := core.Criteria{
		VRAMPerGPUGB: 80, VRAMTotalGB: 80, RAMGB: 256,
		DiskGB: 500, CPUCores: 32, GPUCount: 2, MinNetMbps: 1000,
	}
	want := core.Criteria{
		VRAMPerGPUGB: 12, VRAMTotalGB: 12, RAMGB: 16,
		DiskGB: 40, CPUCores: 4, GPUCount: 1, MinNetMbps: 200,
	}
	got := RaiseCriteria(base, want)
	if !reflect.DeepEqual(got, base) {
		t.Errorf("a smaller requirement lowered the operator's floors:\n got %+v\nwant %+v", got, base)
	}
}

func TestRaiseCriteriaRaisesEachFloor(t *testing.T) {
	want := core.Criteria{
		VRAMPerGPUGB: 24, VRAMTotalGB: 24, RAMGB: 64,
		DiskGB: 200, CPUCores: 8, GPUCount: 1, MinNetMbps: 500,
	}
	got := RaiseCriteria(core.Criteria{}, want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("floors were not raised from an empty base:\n got %+v\nwant %+v", got, want)
	}
}

// Fields the claw says nothing about are the operator's, untouched.
func TestRaiseCriteriaLeavesUnrelatedFieldsAlone(t *testing.T) {
	base := core.Criteria{
		MaxPriceHr: 0.40, CertifiedOnly: true,
		GPUModel: []string{"RTX 4090"}, Interruptible: core.Forbid,
	}
	got := RaiseCriteria(base, core.Criteria{VRAMPerGPUGB: 24})
	if got.MaxPriceHr != 0.40 || !got.CertifiedOnly || len(got.GPUModel) != 1 {
		t.Errorf("unrelated criteria were changed: %+v", got)
	}
	if got.VRAMPerGPUGB != 24 {
		t.Errorf("the claw's own floor was not applied: %+v", got)
	}
}

// ---- result -----------------------------------------------------------

// A teardown reports what it lost. "Four of five saved" is a fact an operator
// can act on; silence is not.
func TestResultSummaryReportsLosses(t *testing.T) {
	r := &Result{Saved: []string{"a"}, Failed: map[string]string{"b": "boom"}}
	if !strings.Contains(r.Summary(), "FAILED") {
		t.Errorf("a loss is invisible in the summary: %q", r.Summary())
	}
	if r.Complete() {
		t.Error("a result with failures reported itself complete")
	}
	if got := (&Result{Failed: map[string]string{}}).Summary(); !strings.Contains(got, "no outputs") {
		t.Errorf("summary = %q", got)
	}
	if !(&Result{}).Complete() {
		t.Error("a result with nothing failed is complete")
	}
	// A local claw returns nil, and nothing downstream may panic on it.
	var nilResult *Result
	if nilResult.Complete() || nilResult.Count() != 0 || nilResult.Summary() == "" {
		t.Error("a nil result is not handled")
	}
}

// ---- config -----------------------------------------------------------

func writeConfig(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A job file that only works from one working directory breaks the first time
// it runs anywhere else — in CI, from an editor, from a sibling directory.
func TestRelativePathsResolveAgainstTheConfigFile(t *testing.T) {
	path := writeConfig(t, "job.yml", "type: demo\nwork: ./sub/w.json\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Resolve("./sub/w.json")
	want := filepath.Join(filepath.Dir(path), "sub", "w.json")
	if got != filepath.Clean(want) {
		t.Errorf("resolved to %q, want %q", got, want)
	}
	// Absolute paths and empty values pass through untouched.
	abs := filepath.Join(string(filepath.Separator), "models", "x.safetensors")
	if cfg.Resolve(abs) != abs {
		t.Error("an absolute path was rewritten")
	}
	if cfg.Resolve("") != "" {
		t.Error("an empty path was rewritten")
	}
}

// The Kind owns everything below `type:`; this package never learns the shape.
func TestDecodeHandsTheDocumentToTheKind(t *testing.T) {
	path := writeConfig(t, "job.yml", "type: demo\nwork: ./w.json\ncount: 3\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	var into struct {
		Work  string `yaml:"work"`
		Count int    `yaml:"count"`
	}
	if err := cfg.Decode(&into); err != nil {
		t.Fatal(err)
	}
	if into.Work != "./w.json" || into.Count != 3 {
		t.Errorf("decoded %+v", into)
	}
}

// A mistyped key in a job file is otherwise a setting that silently does not
// apply, and this is the file that decides what hardware to rent.
func TestAMistypedKeyIsRefusedRatherThanIgnored(t *testing.T) {
	path := writeConfig(t, "job.yml", "type: demo\nwork: ./w.json\nalow_pickle: true\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	var into struct {
		Work        string `yaml:"work"`
		AllowPickle bool   `yaml:"allow_pickle"`
	}
	if err := cfg.Decode(&into); err == nil {
		t.Fatalf("a job file with alow_pickle decoded cleanly as %+v", into)
	} else if !strings.Contains(err.Error(), "alow_pickle") {
		t.Errorf("the error does not name the key: %v", err)
	}
}

// A file saying one thing and a flag saying another means one of them is a
// mistake. Guessing which would run the wrong application against somebody's
// config.
func TestTypeFromFileAndFlagMustAgree(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, "job.yml", "type: demo\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.WithType("other"); err == nil {
		t.Error("a --type disagreeing with the file was accepted")
	}
	if err := cfg.WithType("demo"); err != nil {
		t.Errorf("a --type agreeing with the file was refused: %v", err)
	}
	if err := cfg.WithType(""); err != nil {
		t.Errorf("an absent --type was refused: %v", err)
	}
}

// A file with no type and no flag cannot be run, and the error has to say both
// ways out.
func TestAConfigWithNoTypeIsRefused(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, "job.yml", "work: ./w.json\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.WithType("")
	if err == nil {
		t.Fatal("a config with no type was accepted")
	}
	if !strings.Contains(err.Error(), "--type") {
		t.Errorf("error does not name the remedy: %v", err)
	}
	// The flag alone is enough.
	if err := cfg.WithType("demo"); err != nil {
		t.Errorf("--type did not supply the missing type: %v", err)
	}
}

func TestAnEmptyOrUnreadableConfigIsRefused(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "absent.yml")); err == nil {
		t.Error("a missing config was accepted")
	}
	if _, err := ParseConfig("empty.yml", nil); err == nil {
		t.Error("an empty config was accepted")
	}
	if _, err := ParseConfig("bad.yml", []byte("type: [oh dear\n")); err == nil {
		t.Error("malformed yaml was accepted")
	}
}

// clearRegistry resets global state between tests. The registry is process-wide
// by design — it is populated by package init — so tests that add to it have to
// put it back.
func clearRegistry() { registry = map[Type]Factory{} }
