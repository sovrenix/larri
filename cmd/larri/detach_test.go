// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/daemon"
	pfake "go.sovrenix.com/larri/internal/provider/fake"
	"go.sovrenix.com/larri/internal/rank"
	rfake "go.sovrenix.com/larri/internal/runtime/fake"
	"go.sovrenix.com/larri/internal/sizing"
	"go.sovrenix.com/larri/internal/state"
)

// The test binary stands in for the holder: spawnDetached re-executes the
// running binary, so when it is started as one it plays the part and exits.
func TestMain(m *testing.M) {
	if os.Getenv(detachReadyEnv) != "" {
		if role := os.Getenv("LARRI_TEST_HOLDER"); role != "" {
			playHolder(role)
			return
		}
	}
	os.Exit(m.Run())
}

func playHolder(role string) {
	setupDetachedHolder()
	fmt.Println("  holder     renting")
	if role == "ready-late" {
		// What follows the report is already in the log when the launcher
		// reads the report, which is the worst case a real holder can race
		// into: only the offset keeps it from being shown.
		time.Sleep(300 * time.Millisecond) // several reads of the pipe first
		off, _ := os.Stdout.Seek(0, io.SeekCurrent)
		fmt.Println("  ✓ rig 01TESTRIG READY (the holder's own block)")
		fmt.Println("  holder     holding")
		_ = json.NewEncoder(detachedHolder).Encode(detachReport{OK: true, Rig: "01TESTRIG",
			Endpoint: "http://127.0.0.1:8123/v1", Model: "m", KeyLine: "k", PID: os.Getpid(), LogOffset: off})
		os.Exit(0)
	}
	switch role {
	case "ready":
		fmt.Println("    key: shown to the command that started this rig, and never written here")
		reportDetached(detachReport{OK: true, Rig: "01TESTRIG", Endpoint: "http://127.0.0.1:8123/v1",
			Model: "m", Hardware: "fake A40 48GB", PriceHr: 0.49,
			KeyLine: "SECRET-KEY-VALUE   (this rig only — shown once)", Key: "SECRET-KEY-VALUE"})
		fmt.Println("  ✓ rig 01TESTRIG READY (the holder's own block)")
		fmt.Println("  holder     holding")
		os.Exit(0)
	case "unsatisfiable":
		reportDetached(detachReport{Error: "daemon.survey: criteria-unsatisfiable: no offer", Class: "criteria-unsatisfiable"})
		os.Exit(1)
	case "dies":
		fmt.Println("  holder     no offer satisfies the criteria")
		os.Exit(1) // without a report, as a crash would
	}
}

func holderEnv(t *testing.T, role string) string {
	dir := t.TempDir()
	t.Setenv("LARRI_STATE_DIR", dir)
	t.Setenv("LARRI_TEST_HOLDER", role)
	return dir
}

// The launching command returns once the holder reports ready, and the key
// reaches this terminal only through the pipe: the holder's log, which
// outlives the command, never carries it.
func TestADetachedHolderReportsReadyAndLogsNoKey(t *testing.T) {
	dir := holderEnv(t, "ready")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := spawnDetached(ctx, "up", nil, true); err != nil {
		t.Fatalf("a ready holder failed the launch: %v", err)
	}
	logs, _ := filepath.Glob(filepath.Join(dir, "logs", "up-*.log"))
	if len(logs) != 1 {
		t.Fatalf("logs = %v", logs)
	}
	deadline := time.Now().Add(5 * time.Second)
	var body []byte
	for time.Now().Before(deadline) {
		body, _ = os.ReadFile(logs[0])
		if strings.Contains(string(body), "holding") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Contains(string(body), "SECRET-KEY-VALUE") {
		t.Error("the key was written to the holder's log")
	}
	if fi, _ := os.Stat(logs[0]); fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode %o, want 600", fi.Mode().Perm())
	}
}

// A holder that dies before it is ready fails the command that started it,
// and says where to look.
func TestAHolderThatDiesFailsTheLaunch(t *testing.T) {
	holderEnv(t, "dies")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := spawnDetached(ctx, "up", nil, true)
	if err == nil || !strings.Contains(err.Error(), "ended before the rig was ready") ||
		!strings.Contains(err.Error(), "log ") {
		t.Errorf("err = %v; want the failure and the log path", err)
	}
}

// Without a terminal there is no one to confirm a rental, so a detached one
// needs --yes rather than implying it; and a dry run has nothing to hold.
func TestADetachedRentalIsConfirmedOrRefused(t *testing.T) {
	o := &daemon.Orchestrator{}
	req := daemon.UpRequest{Model: core.ModelSpec{Ref: "m"}}
	if err := launchDetachedUp(context.Background(), o, req, nil, detachOptions{}); err == nil ||
		!strings.Contains(err.Error(), "needs --yes") {
		t.Errorf("no terminal, no --yes: err = %v", err)
	}
	if err := launchDetachedUp(context.Background(), o, req, nil, detachOptions{dryRun: true, yes: true}); err == nil ||
		!strings.Contains(err.Error(), "dry-run") {
		t.Errorf("dry run: err = %v", err)
	}
}

// Flag conflicts are refused before anything is printed or looked up. Resume
// rents nothing, so it needs no --yes; --json means nothing without --detach.
func TestDetachFlagConflictsAreRefusedFirst(t *testing.T) {
	for _, c := range []struct {
		name                                   string
		command                                string
		detach, json, dryRun, yes, interactive bool
		want                                   string
	}{
		{"json alone", "up", false, true, false, true, true, "--json needs --detach"},
		{"dry run", "up", true, false, true, true, true, "--dry-run"},
		{"no terminal", "up", true, false, false, false, false, "needs --yes"},
		{"no terminal, --yes", "up", true, true, false, true, false, ""},
		{"terminal", "up", true, false, false, false, true, ""},
		{"resume", "resume", true, true, false, true, true, ""},
		{"foreground", "up", false, false, true, false, false, ""},
	} {
		err := checkDetach(c.command, c.detach, c.json, c.dryRun, c.yes, c.interactive)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestDetachFlagsAreNotPassedToTheHolder(t *testing.T) {
	got := withoutFlags([]string{"--model", "m", "-d", "--detach=true", "--json", "--yes"}, "d", "detach", "json")
	if strings.Join(got, " ") != "--model m --yes" {
		t.Errorf("holder args = %v", got)
	}
}

// What the launcher can settle it settles itself, rather than starting a
// process to discover it: a rig another larri process holds is refused by
// name, and nothing is spawned or logged.
func TestResumeDetachedRefusesAHeldRigWithoutSpawning(t *testing.T) {
	dir := holderEnv(t, "ready")
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := state.NewID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rig := &core.Rig{ID: id, State: core.StateReady, CreatedAt: time.Now().UTC(),
		Instance: &core.Instance{Provider: "fake", InstanceID: "i-1"}}
	if err := st.Save(rig); err != nil {
		t.Fatal(err)
	}
	release, err := st.Hold(id, state.Holder{PID: 5150, Detached: true})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	err = cmdResume(context.Background(), []string{"-d"})
	if err == nil || !strings.Contains(err.Error(), "already served by larri pid 5150") {
		t.Fatalf("err = %v; want the holder named", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs")); !os.IsNotExist(err) {
		t.Error("a holder was started for a rig that was already held")
	}
}

// A port that is taken is refused before the launcher asks to spend: the
// holder would refuse it anyway, after the operator had said yes.
func TestADetachedLaunchChecksThePortBeforeAsking(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	taken := ln.Addr().(*net.TCPAddr).Port
	prompts := make(chan cliPrompt, 1)
	o := &daemon.Orchestrator{} // no provider: anything past the check would panic
	req := daemon.UpRequest{Model: core.ModelSpec{Ref: "m"}, LocalPort: taken}
	err = launchDetachedUp(context.Background(), o, req, nil,
		detachOptions{interactive: true, prompts: prompts})
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("err = %v; want the taken port refused", err)
	}
	if len(prompts) != 0 {
		t.Error("asked to rent with the port taken")
	}
}

// A holder held to the agreed price can run out of offers where a foreground
// fallback would have asked about the next one. Its message names the criteria;
// the launcher, which set the ceiling, says so.
func TestAHolderOutOfOffersAtTheAgreedPriceIsExplained(t *testing.T) {
	holderEnv(t, "unsatisfiable")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := spawnDetached(ctx, "up", nil, true)
	var f *detachFailure
	if !errors.As(err, &f) || f.Class != "criteria-unsatisfiable" {
		t.Fatalf("err = %#v; want the holder's class carried back", err)
	}
	if note := heldToAgreed(err, 0.0705); !strings.Contains(note, "$0.070/hr you agreed to") {
		t.Errorf("note = %q", note)
	}
	if note := heldToAgreed(err, 0); note != "" {
		t.Errorf("with --yes nothing was agreed, and the note says %q", note)
	}
	if note := heldToAgreed(errors.New("x"), 0.07); note != "" {
		t.Errorf("an unrelated failure got the note %q", note)
	}
}

// The launcher shows the holder's log up to the report and no further, so the
// holder's own READY block — written to its log after it reports — is not
// shown a second time beneath the launcher's.
func TestTheLauncherShowsTheLogOnlyUpToTheReport(t *testing.T) {
	holderEnv(t, "ready-late")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out := captureStdout(t, func() {
		if err := spawnDetached(ctx, "up", nil, false); err != nil {
			t.Errorf("launch failed: %v", err)
		}
	})
	if !strings.Contains(out, "holder     renting") {
		t.Errorf("the log before the report was not shown:\n%s", out)
	}
	if strings.Contains(out, "the holder's own block") || strings.Contains(out, "holding") {
		t.Errorf("the log after the report was shown:\n%s", out)
	}
	if strings.Count(out, "READY") != 1 {
		t.Errorf("READY shown %d times:\n%s", strings.Count(out, "READY"), out)
	}
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	f()
	os.Stdout = orig
	w.Close()
	return <-done
}

// With --json, stdout carries the one object and nothing else: a script reads
// it with a JSON parser, and the notices a person needs go to stderr.
func TestJSONIsTheOnlyThingOnStdout(t *testing.T) {
	holderEnv(t, "ready")
	origOut, origReport := os.Stdout, reportOut
	defer func() { os.Stdout, reportOut = origOut, origReport }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	jsonOnStdout()
	fmt.Println("  privacy     a notice for a person") // now on stderr
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := spawnDetached(ctx, "up", nil, true); err != nil {
		t.Fatal(err)
	}
	w.Close()
	out, _ := io.ReadAll(r)
	var rep detachReport
	dec := json.NewDecoder(strings.NewReader(string(out)))
	if err := dec.Decode(&rep); err != nil || !rep.OK {
		t.Fatalf("stdout is not one JSON report: %v\n%s", err, out)
	}
	if dec.More() {
		t.Errorf("stdout carries more than the report:\n%s", out)
	}
	if strings.Contains(string(out), "privacy") {
		t.Errorf("a notice reached stdout:\n%s", out)
	}
}

// A launcher that fails before it starts a holder still answers --json with an
// object; one whose holder failed has already printed it, and does not twice.
func TestAJSONLauncherReportsItsOwnFailureOnce(t *testing.T) {
	origOut, origReport, origMode := os.Stdout, reportOut, jsonMode
	defer func() { os.Stdout, reportOut, jsonMode = origOut, origReport, origMode }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	jsonOnStdout()
	reportFailure(errors.New("daemon.UpAndServe: wiring: local port 8000 is already in use"))
	reportFailure(&detachFailure{detachReport{Error: "reported already"}})
	w.Close()
	out, _ := io.ReadAll(r)
	if got := strings.Count(string(out), "\n"); got != 1 || !strings.Contains(string(out), `"ok":false`) ||
		!strings.Contains(string(out), "already in use") {
		t.Errorf("stdout = %q; want one failure object", out)
	}
}

// swapStdout gives a test the process's stdout, and restores it and the JSON
// reporting state afterwards.
func swapStdout(t *testing.T) (read func() string) {
	t.Helper()
	origOut, origReport, origMode := os.Stdout, reportOut, jsonMode
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout, reportOut, jsonMode = origOut, origReport, origMode })
	return func() string {
		os.Stdout = origOut
		w.Close()
		b, _ := io.ReadAll(r)
		return string(b)
	}
}

// A refused flag is still an outcome --json owes an object for: the check ran
// before stdout was given over to the report, so the refusal went to stderr and
// stdout stayed empty.
func TestARefusedDetachStillAnswersJSON(t *testing.T) {
	t.Setenv("LARRI_STATE_DIR", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	read := swapStdout(t)
	err := cmdUp(context.Background(), []string{"-d", "--json", "--model", "test/model"}) // no terminal, no --yes
	if err == nil {
		t.Fatal("rented detached without --yes and without a terminal")
	}
	reportFailure(err)
	out := read()
	var rep detachReport
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &rep); jerr != nil || rep.OK || !strings.Contains(rep.Error, "needs --yes") {
		t.Errorf("stdout %q; want one failure object", out)
	}
}

// Declining the offer is an outcome too, and with --json it is reported as one.
func TestADeclinedDetachedRentalAnswersJSON(t *testing.T) {
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	market := []core.Offer{{Provider: "fake", OfferID: "a", GPUModel: "RTX 4090", GPUCount: 1,
		VRAMPerGPUGB: 24, PriceHr: 0.40, Reliability: 0.97, MachineID: "m1"}}
	o := &daemon.Orchestrator{
		Store: st, Provider: pfake.New("fake", market, pfake.Behaviour{}), Runtime: rfake.New(rfake.Behaviour{}),
		Resolver: sizing.StaticResolver{"test/model": sizing.Facts{Ref: "test/model", Params: 8.03, Layers: 32,
			KVHeads: 8, HeadDim: 128, HiddenSize: 4096, MaxContextLen: 131072}},
		Policy: rank.DefaultPolicy(),
	}
	req := daemon.UpRequest{Model: core.ModelSpec{Ref: "test/model", ServedName: "test",
		Quantization: "q4_K_M", ContextLen: 8192}, DiskGB: 50}
	prompts := make(chan cliPrompt)
	go func() {
		for p := range prompts {
			p.Result <- false
		}
	}()
	defer close(prompts)
	read := swapStdout(t)
	jsonOnStdout()
	if err := launchDetachedUp(context.Background(), o, req, nil,
		detachOptions{interactive: true, json: true, prompts: prompts}); err != nil {
		t.Fatal(err)
	}
	out := read()
	var rep detachReport
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &rep); jerr != nil || rep.OK || !strings.Contains(rep.Error, "declined") {
		t.Errorf("stdout %q; want the decline reported as one object", out)
	}
}

// A detached holder's interrupt is a signal no one typed, and the record of
// the teardown says so.
func TestAnInterruptedTeardownSaysWhoInterrupted(t *testing.T) {
	orig := detachedMode
	defer func() { detachedMode = orig }()
	detachedMode = false
	if s := interruptedTermination().Summary; !strings.Contains(s, "interrupted from the CLI") {
		t.Errorf("foreground: %q", s)
	}
	detachedMode = true
	if s := interruptedTermination().Summary; !strings.Contains(s, "detached larri process holding it was stopped") {
		t.Errorf("detached: %q", s)
	}
}
