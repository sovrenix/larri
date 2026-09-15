// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.sovrenix.com/larri/internal/clientkeys"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/errs"
)

// A detached rig is held by a larri process of its own, started by `larri up -d`
// or `larri resume -d` and left running when that command returns.
//
// Holding a rig is a live process — a tunnel, a proxy on the local port, a
// supervisor — so a foreground `up` blocks whoever started it, which for an
// agent driving a shell is a command that never returns. Detaching moves that
// process out of the way and changes nothing about what it does: the same
// orchestrator, the same supervision, idle reclamation and budget, the same
// `larri down`.
//
// The launching command confirms and reports; the detached one rents and
// holds. They talk over one inherited pipe, on which the holder writes a single
// report when the rig is ready or the bring-up has failed. Its output goes to a
// log that never carries a key value: the key reaches only the terminal that
// asked for the rig, through that pipe.

const (
	detachReadyEnv = "LARRI_DETACHED_READY_FD" // set in the holder: the report pipe's descriptor
	detachLogEnv   = "LARRI_DETACHED_LOG"      // set in the holder: where its output goes
)

// detachReport is what the holder tells the command that started it.
type detachReport struct {
	OK       bool    `json:"ok"`
	Rig      string  `json:"rig,omitempty"`
	Endpoint string  `json:"endpoint,omitempty"`
	Model    string  `json:"model,omitempty"`
	Hardware string  `json:"hardware,omitempty"`
	PriceHr  float64 `json:"price_hr,omitempty"`
	KeyLine  string  `json:"key_line,omitempty"`
	Key      string  `json:"key,omitempty"` // only when a key is being shown for the first time
	PID      int     `json:"pid"`
	Log      string  `json:"log"`
	Error    string  `json:"error,omitempty"`
	Class    string  `json:"class,omitempty"` // the error's class, e.g. criteria-unsatisfiable

	// LogOffset is how much of the log the holder had written when it
	// reported. The launcher shows the log up to there and no further, so
	// what the holder prints after — its own READY block — is not shown twice.
	LogOffset int64 `json:"log_offset,omitempty"`
}

// detachFailure is a holder's failure as the launcher reports it, with the
// class kept so the launcher can say what only it knows.
type detachFailure struct{ detachReport }

func (f *detachFailure) Error() string {
	return strings.TrimSpace(f.detachReport.Error) + ": log " + f.Log
}

// reportOut is where a launcher run with --json writes its one object. Every
// other line the launcher prints goes to stderr instead (jsonOnStdout), so a
// script reading stdout reads JSON and nothing else.
var (
	reportOut = os.Stdout
	jsonMode  bool
)

// jsonOnStdout keeps stdout for the JSON report. The first-run notice, the
// privacy headline and the rest are still printed — to stderr, where a person
// running the command still sees them.
func jsonOnStdout() {
	reportOut = os.Stdout
	os.Stdout = os.Stderr
	jsonMode = true
}

// reportFailure gives a launcher run with --json its object when it fails
// itself — a taken port, a declined flag — rather than an empty stdout. A
// holder's failure has already been reported, by finishDetached.
func reportFailure(err error) {
	var f *detachFailure
	if !jsonMode || errors.As(err, &f) {
		return
	}
	r := detachReport{Error: err.Error()}
	if c := errs.ClassOf(err); c != errs.ClassUnknown {
		r.Class = c.String()
	}
	_ = json.NewEncoder(reportOut).Encode(r)
}

// detachedHolder is the report pipe when this process is a detached holder
// that has not yet reported; detachedMode stays true after it has.
var (
	detachedHolder *os.File
	detachedMode   bool
)

// setupDetachedHolder recognises a detached holder and prepares it: no
// controlling terminal to lose, and no hang-up to die of.
func setupDetachedHolder() {
	if os.Getenv(detachReadyEnv) == "" {
		return
	}
	detachedHolder = os.NewFile(3, "larri-ready")
	detachedMode = true
	signal.Ignore(syscall.SIGHUP)
}

// reportDetached sends the holder's one report, if this process is a holder
// that has not yet sent it.
func reportDetached(r detachReport) {
	if detachedHolder == nil {
		return
	}
	r.PID = os.Getpid()
	r.Log = os.Getenv(detachLogEnv)
	if off, err := os.Stdout.Seek(0, io.SeekCurrent); err == nil {
		r.LogOffset = off
	}
	_ = json.NewEncoder(detachedHolder).Encode(r)
	_ = detachedHolder.Close()
	detachedHolder = nil
}

// withoutFlags removes the named boolean flags from an argument list, in any
// of the spellings the flag package accepts.
func withoutFlags(args []string, names ...string) []string {
	drop := map[string]bool{}
	for _, n := range names {
		for _, dash := range []string{"-", "--"} {
			drop[dash+n] = true
			drop[dash+n+"=true"] = true
			drop[dash+n+"=false"] = true
		}
	}
	out := make([]string, 0, len(args))
	for _, a := range args {
		if !drop[a] {
			out = append(out, a)
		}
	}
	return out
}

// checkDetach refuses what a detached launch cannot honour, before anything
// is printed, configured, or searched: a refusal found after the first-run
// preamble and the privacy notice is found too late to read.
func checkDetach(command string, detach, jsonOut, dryRun, yes, interactive bool) error {
	switch {
	case jsonOut && !detach:
		return fmt.Errorf("%s: --json needs --detach", command)
	case !detach || detachedHolder != nil:
		return nil
	case dryRun:
		return fmt.Errorf("%s: --detach with --dry-run: nothing is rented to hold", command)
	case !interactive && !yes:
		return fmt.Errorf("%s: --detach without a terminal needs --yes: a detached rig is rented without a prompt", command)
	}
	return nil
}

// spawnDetached starts `larri <command> <args>` as a detached holder, follows
// its log until it reports, and returns. A failure before the rig is ready is
// this command's failure; after it, the holder is on its own.
func spawnDetached(ctx context.Context, command string, args []string, jsonOut bool) error {
	logDir := filepath.Join(stateDir(), "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return fmt.Errorf("detach: %w", err)
	}
	logPath := filepath.Join(logDir, fmt.Sprintf("%s-%s-%d.log", command,
		time.Now().UTC().Format("20060102T150405Z"), os.Getpid()))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("detach: %w", err)
	}
	readR, readW, err := os.Pipe()
	if err != nil {
		logFile.Close()
		return fmt.Errorf("detach: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		logFile.Close()
		return fmt.Errorf("detach: %w", err)
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		logFile.Close()
		return fmt.Errorf("detach: %w", err)
	}
	cmd := exec.Command(exe, append([]string{command}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, logFile, logFile
	cmd.ExtraFiles = []*os.File{readW} // descriptor 3 in the holder
	cmd.Env = append(os.Environ(), detachReadyEnv+"=3", detachLogEnv+"="+logPath)
	// Its own session: no controlling terminal, so closing this one does not
	// reach it, and a Ctrl-C here is this command's to pass on or not.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("detach: start holder: %w", err)
	}
	readW.Close()
	logFile.Close()
	devnull.Close()
	if !jsonOut {
		doing := "is renting"
		if command == "resume" {
			doing = "is reconnecting"
		}
		fmt.Printf("  detached    larri pid %d %s and will hold the rig — log %s\n\n", cmd.Process.Pid, doing, logPath)
	}

	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	defer readR.Close()

	follow, _ := os.Open(logPath)
	if follow != nil {
		defer follow.Close()
	}
	var shown int64
	showLogTo := func(limit int64) {
		if follow == nil || jsonOut || limit <= shown {
			return
		}
		n, _ := io.CopyN(os.Stdout, follow, limit-shown)
		shown += n
	}
	logSize := func() int64 {
		if fi, err := os.Stat(logPath); err == nil {
			return fi.Size()
		}
		return shown
	}

	// The pipe is read here rather than by a goroutine, so the log is never
	// shown past the report. The log's size is taken before each read: if
	// that read finds no report, the holder had not reported when the size
	// was taken, and so had not yet printed anything that follows its report.
	var pending []byte
	buf := make([]byte, 8192)
	interrupted := false
	for {
		if ctx.Err() != nil && !interrupted {
			// Ctrl-C before the rig is ready ends the bring-up, as it does in
			// the foreground: the holder tears down what it made. Asked once;
			// the report still arrives when it has.
			interrupted = true
			_ = cmd.Process.Signal(syscall.SIGTERM)
			if !jsonOut {
				fmt.Println("\n  interrupted — the detached process is tearing down what it rented")
			}
		}
		size := logSize()
		_ = readR.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, err := readR.Read(buf)
		pending = append(pending, buf[:n]...)
		if i := bytes.IndexByte(pending, '\n'); i >= 0 {
			var r detachReport
			if json.Unmarshal(pending[:i], &r) != nil {
				r = detachReport{Error: "the detached larri process sent an unreadable report"}
			}
			if r.LogOffset > 0 {
				showLogTo(r.LogOffset)
			} else {
				showLogTo(logSize())
			}
			r.LogOffset = 0
			return finishDetached(r, cmd.Process.Pid, logPath, jsonOut, exited)
		}
		switch {
		case n > 0:
			// Part of a report: the rest follows.
		case err == nil || errors.Is(err, os.ErrDeadlineExceeded):
			showLogTo(size)
		default:
			// The pipe closed with no report: the holder has gone.
			showLogTo(logSize())
			return finishDetached(detachReport{Error: "the detached larri process ended before the rig was ready"},
				cmd.Process.Pid, logPath, jsonOut, exited)
		}
	}
}

// finishDetached reports the holder's outcome.
func finishDetached(r detachReport, pid int, logPath string, jsonOut bool, exited <-chan struct{}) error {
	if r.Log == "" {
		r.Log = logPath
	}
	if r.PID == 0 {
		r.PID = pid
	}
	if jsonOut {
		_ = json.NewEncoder(reportOut).Encode(r)
	}
	if !r.OK {
		// Let a failing holder finish its teardown before this command says
		// it has failed, so `larri status` right after reads the outcome.
		select {
		case <-exited:
		case <-time.After(3 * time.Minute):
		}
		if r.Error == "" {
			r.Error = "bring-up failed"
		}
		return &detachFailure{r}
	}
	if jsonOut {
		return nil
	}
	fmt.Printf("\n  ✓ rig %s READY   %s   model: %s\n", r.Rig, r.Endpoint, r.Model)
	fmt.Printf("    %s at $%.3f/hr\n", r.Hardware, r.PriceHr)
	fmt.Printf("    key: %s\n", r.KeyLine)
	fmt.Printf("\n  detached: larri pid %d holds the rig and supervises it — log %s\n", r.PID, r.Log)
	fmt.Printf("  larri status shows it; larri down %s ends it and stops the bill\n", r.Rig)
	return nil
}

// detachOptions are the parts of `larri up` a detached launch needs.
type detachOptions struct {
	interactive, yes, dryRun, json bool
	maxPrice                       float64
	prompts                        chan<- cliPrompt
}

// launchDetachedUp confirms a rental and starts the holder that makes it.
//
// Confirmation stays here, where the terminal is. The survey runs in this
// process with the ordinary prompt, and the holder is told the price that was
// agreed to as a ceiling: it searches again, and a market that moved in between
// must not rent something dearer than the operator said yes to. Without a
// terminal there is no one to ask, so --yes is required rather than implied.
func launchDetachedUp(ctx context.Context, o *daemon.Orchestrator, req daemon.UpRequest,
	args []string, opt detachOptions) error {

	if err := checkDetach("up", true, opt.json, opt.dryRun, opt.yes, opt.interactive); err != nil {
		return err
	}
	// The holder checks the port before it rents, as every bring-up does; the
	// launcher checks it before it asks. A live run confirmed a purchase here
	// and then watched the holder refuse it over a port that was taken all
	// along.
	if err := daemon.CheckLocalPort(req.LocalPort); err != nil {
		return err
	}
	if err := o.CheckClientKeys(); err != nil {
		return err
	}
	childArgs := withoutFlags(args, "d", "detach", "json")
	var agreed float64
	if !opt.yes {
		req.Confirm = func(of core.Offer, _ core.SizingPlan) bool {
			result := make(chan bool)
			opt.prompts <- cliPrompt{Offer: of, Result: result}
			if <-result {
				agreed = of.PriceHr
			}
			return false // the holder rents, not this process
		}
		if _, err := o.Up(ctx, req); err != nil && !errors.Is(err, daemon.ErrConfirmationDeclined) {
			return err
		}
		if agreed == 0 {
			fmt.Println("\n  not rented")
			if opt.json {
				_ = json.NewEncoder(reportOut).Encode(detachReport{Error: "not rented: the offer was declined"})
			}
			return nil
		}
		ceiling := agreed + 0.0005 // the quote to the tenth of a cent, and no further
		if opt.maxPrice > 0 && opt.maxPrice < ceiling {
			ceiling = opt.maxPrice
		}
		childArgs = append(childArgs, "--max-price", strconv.FormatFloat(ceiling, 'f', 4, 64))
	}
	childArgs = append(childArgs, "--yes")
	err := spawnDetached(ctx, "up", childArgs, opt.json)
	if note := heldToAgreed(err, agreed); note != "" && !opt.json {
		fmt.Printf("\n  %s\n", note)
	}
	return err
}

// heldToAgreed explains a detached rental that ran out of offers. A foreground
// fallback asks again before renting the next offer; a holder cannot ask, so
// it is held to the price agreed to, and when the offer agreed to fails there
// may be nothing at that price to fall back on. The holder's own message
// names the criteria and not the ceiling, which only the launcher knows it set.
func heldToAgreed(err error, agreed float64) string {
	var f *detachFailure
	if agreed <= 0 || !errors.As(err, &f) || f.Class != errs.ClassCriteriaUnsatisfiable.String() {
		return ""
	}
	return fmt.Sprintf("the detached rental was held to the $%.3f/hr you agreed to, and nothing at that "+
		"price was left to fall back on — larri up -d again to be asked about the next offer", agreed)
}

// readyKey settles which key a serving rig's clients use, once, and returns the
// line to print about it. A detached holder sends the key to the command that
// started it instead, and does so before printing anything about readiness, so
// that command shows the holder's log up to the report and its own READY block
// after it — one block, not two.
func readyKey(live *daemon.Live, keys *clientkeys.Store, hardware string, priceHr float64) (string, error) {
	value, line, err := keyInfo(live, keys)
	if detachedHolder == nil {
		return line, err
	}
	if err != nil {
		line = "! " + err.Error()
	}
	reportDetached(detachReport{
		OK: true, Rig: live.Rig.ID, Endpoint: live.Endpoint, Model: live.Rig.Model.ServedName,
		Hardware: hardware, PriceHr: math.Round(priceHr*10000) / 10000, KeyLine: line, Key: value,
	})
	return "shown to the command that started this rig, and never written here", nil
}

// printKey prints the line readyKey returned.
func printKey(line string, err error) {
	if err != nil {
		fmt.Printf("    ! key: %v\n", err)
		return
	}
	fmt.Printf("    key: %s\n", line)
}

// holdingLine says how the rig this process holds is ended.
func holdingLine(rigID string) string {
	if detachedMode {
		return fmt.Sprintf("holding the tunnel detached — larri down %s tears down and stops paying", rigID)
	}
	return "holding the tunnel — Ctrl-C to tear down and stop paying"
}
