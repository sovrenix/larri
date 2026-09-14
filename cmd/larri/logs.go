// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"syscall"
	"time"

	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/state"
)

const logsUsage = `larri logs — what LARRI wrote while holding a rig

  larri logs [-f] [-n N] [rig]

  rig            a rig id, or enough of it to be unambiguous
                 (default: the newest rig with a log)
  -f, --follow   keep printing while a larri process holds the rig
  -n, --tail N   show only the last N lines (default: all)

A rig brought up or resumed with -d has a log: its bring-up, supervision and
teardown, from every process that has held it. A foreground larri up writes to
its terminal instead. The inference engine's own log is on the host, and
larri_logs reads it from a rig the MCP server is serving.
`

// cmdLogs prints a rig's holder logs, the way `docker logs` prints a
// container's.
func cmdLogs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	fs.Usage = func() { fmt.Print(logsUsage) }
	follow := fs.Bool("follow", false, "keep printing while a larri process holds the rig")
	fs.BoolVar(follow, "f", false, "shorthand for --follow")
	tail := fs.String("tail", "all", "show only the last N lines")
	fs.StringVar(tail, "n", "all", "shorthand for --tail")
	refs := parseInterleaved(fs, args)
	if len(refs) > 1 {
		return errors.New("logs: one rig at a time: larri logs [-f] [-n N] [rig]")
	}
	lines := -1
	if *tail != "all" {
		n, err := strconv.Atoi(*tail)
		if err != nil || n < 0 {
			return fmt.Errorf("logs: --tail %q: expected a number of lines, or all", *tail)
		}
		lines = n
	}
	ref := ""
	if len(refs) == 1 {
		ref = refs[0]
	}

	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	l, err := daemon.FindRigLogs(st, ref)
	if err != nil {
		return err
	}
	text, offset, err := l.Read(lines)
	if err != nil {
		return err
	}
	fmt.Print(text)
	if !*follow || len(l.Paths) == 0 {
		return nil
	}
	return followLogs(ctx, st, l, offset, os.Stdout, 250*time.Millisecond)
}

// parseInterleaved parses flags wherever they fall among the arguments —
// `larri logs 01K… -f` as well as `larri logs -f 01K…` — and returns the rest.
// The flag package stops at the first argument that is not a flag.
func parseInterleaved(fs *flag.FlagSet, args []string) []string {
	var rest []string
	for {
		_ = fs.Parse(args)
		if fs.NArg() == 0 {
			return rest
		}
		rest = append(rest, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// followLogs prints what the rig's holder writes, from offset in its log, until
// no process holds the rig and the last holder has exited — the release comes
// before the lines that say how the rig ended — or until interrupted. When
// another process takes the rig, it follows that one's log.
func followLogs(ctx context.Context, st *state.Store, l daemon.RigLogs, offset int64,
	out io.Writer, every time.Duration) error {

	path, pid := l.Paths[len(l.Paths)-1], l.Holder.PID
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		offset = copyLogFrom(out, path, offset)
		h, held, err := st.HolderOf(l.Rig)
		switch {
		case err != nil:
		case held && h.PID != pid && h.Log == "":
			fmt.Fprintf(out, "── larri pid %d holds the rig now, in the foreground: its output goes to its terminal\n", h.PID)
			return nil
		case held && h.PID != pid:
			fmt.Fprintf(out, "── larri pid %d holds the rig now: %s\n", h.PID, h.Log)
			path, pid, offset = h.Log, h.PID, 0
			continue
		case !held && !processAlive(pid):
			copyLogFrom(out, path, offset)
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// copyLogFrom writes what the log holds past offset and returns the new end.
func copyLogFrom(out io.Writer, path string, offset int64) int64 {
	f, err := os.Open(path)
	if err != nil {
		return offset
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() < offset {
		offset = 0 // replaced under us: start again
	}
	n, _ := io.Copy(out, io.NewSectionReader(f, offset, 1<<62))
	return offset + n
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
