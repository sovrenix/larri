// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"go.sovrenix.com/larri/internal/state"
)

// RigLogs is what LARRI wrote while holding one rig: the output of every
// detached process that held it, oldest first.
//
// It is LARRI's account of the rig — bring-up, supervision, teardown — not the
// inference engine's. The engine's log is on the host and is read over the
// holder's SSH session, which another process does not have (larri_logs reads
// it from a rig the MCP server itself is serving).
type RigLogs struct {
	Rig    string
	Holder state.Holder // the rig's current holder, or its last one
	Held   bool
	Paths  []string // oldest first; the last is the current or latest holder's
}

// maxLogRead bounds how much of one log is read. A holder writes a line per
// event, so a log this large is one that has run for days; its end is what
// anyone asking wants.
const maxLogRead = 8 << 20

// FindRigLogs resolves a rig by its id or a unique prefix of it — or, given
// none, the newest rig a detached holder wrote a log for — and finds its logs.
func FindRigLogs(st *state.Store, ref string) (RigLogs, error) {
	var ids []string
	if ref != "" {
		id, err := ResolveRig(st, ref)
		if err != nil {
			return RigLogs{}, err
		}
		ids = []string{id}
	} else {
		rigs, err := st.List() // newest first
		if err != nil {
			return RigLogs{}, err
		}
		for _, r := range rigs {
			ids = append(ids, r.ID)
		}
	}
	for _, id := range ids {
		h, held, err := st.HolderOf(id)
		if err != nil {
			return RigLogs{}, err
		}
		l := RigLogs{Rig: id, Holder: h, Held: held, Paths: h.Logs()}
		if len(l.Paths) > 0 {
			return l, nil
		}
		if ref == "" {
			continue // the newest rig with a log, when none was named
		}
		switch {
		case held:
			return l, fmt.Errorf("logs: rig %s has no log: larri pid %d holds it in the foreground and writes to its terminal", id, h.PID)
		case h.PID != 0:
			return l, fmt.Errorf("logs: rig %s has no log: larri pid %d held it in the foreground and wrote to its terminal", id, h.PID)
		default:
			return l, fmt.Errorf("logs: rig %s has no log: no detached larri process has held it", id)
		}
	}
	return RigLogs{}, errors.New("logs: no rig has a log: a detached larri up -d or larri resume -d writes one")
}

// ResolveRig names a rig by its id or a unique prefix of it, in either case.
func ResolveRig(st *state.Store, ref string) (string, error) {
	rigs, err := st.List()
	if err != nil {
		return "", err
	}
	want := strings.ToUpper(ref)
	var ids []string
	for _, r := range rigs {
		if r.ID == want {
			return r.ID, nil
		}
		if strings.HasPrefix(r.ID, want) {
			ids = append(ids, r.ID)
		}
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("logs: no rig %q: larri status --all lists them", ref)
	case 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("logs: %q matches %d rigs: give more of the id", ref, len(ids))
}

// Read returns the rig's logs, oldest first, cut to their last n lines — all of
// them when n is negative, none when it is zero, as `docker logs --tail` counts
// — and how many bytes of the latest log it read, which is where a reader
// following the log carries on from.
func (l RigLogs) Read(n int) (text string, latestOffset int64, err error) {
	var b bytes.Buffer
	for i, path := range l.Paths {
		body, size, err := readLog(path)
		if len(l.Paths) > 1 {
			fmt.Fprintf(&b, "── %s\n", path)
		}
		if err != nil {
			fmt.Fprintf(&b, "   (not readable: %v)\n", err)
			continue
		}
		b.Write(body)
		if len(body) > 0 && body[len(body)-1] != '\n' && i < len(l.Paths)-1 {
			b.WriteByte('\n')
		}
		if i == len(l.Paths)-1 {
			latestOffset = size
		}
	}
	return lastLines(b.String(), n), latestOffset, nil
}

// readLog reads a log, or its last maxLogRead bytes, and says how long it is.
func readLog(path string) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := fi.Size()
	start := int64(0)
	if size > maxLogRead {
		start = size - maxLogRead
	}
	body, err := io.ReadAll(io.NewSectionReader(f, start, size-start))
	return body, size, err
}

// lastLines keeps the last n lines of s: all of it when n is negative.
func lastLines(s string, n int) string {
	if n < 0 || s == "" {
		return s
	}
	if n == 0 {
		return ""
	}
	end := len(s)
	if s[end-1] == '\n' {
		end--
	}
	for i := end - 1; i >= 0; i-- {
		if s[i] == '\n' {
			n--
			if n == 0 {
				return s[i+1:]
			}
		}
	}
	return s
}
