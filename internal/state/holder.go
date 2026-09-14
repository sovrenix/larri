// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Holder is the process holding a rig: the one serving its local endpoint and
// supervising it.
//
// A rig has at most one. Two holders mean two tunnels, two supervisors, and
// two idle clocks that each think the other's traffic is not operator traffic
// — and nothing prevented it: `larri resume` would adopt a rig a detached
// `larri up` was still serving.
type Holder struct {
	PID      int       `json:"pid"`
	Started  time.Time `json:"started"`
	Log      string    `json:"log,omitempty"` // where a detached holder writes its output
	Detached bool      `json:"detached,omitempty"`

	// EarlierLogs are the logs of the rig's earlier holders, oldest first. A
	// rig resumed after its holder died would otherwise lose the one log that
	// says what happened before it did.
	EarlierLogs []string `json:"earlier_logs,omitempty"`
}

// maxEarlierLogs bounds the history a hold record carries.
const maxEarlierLogs = 32

// Logs is every log the rig's holders wrote, oldest first.
func (h Holder) Logs() []string {
	logs := append([]string(nil), h.EarlierLogs...)
	if h.Log != "" {
		logs = append(logs, h.Log)
	}
	return logs
}

// ErrHeld is returned when a rig already has a holder.
var ErrHeld = errors.New("rig is held by another larri process")

func (s *Store) holdPath(id string) string {
	return filepath.Join(s.dir, "rigs", "."+id+".hold")
}

// Hold takes the rig for this process. It is held until release is called or
// the process ends: the lock is the kernel's, so a holder that crashes or is
// killed frees the rig without anyone cleaning up after it.
//
// The record stays after release, so the last holder's log can still be found.
func (s *Store) Hold(id string, h Holder) (release func(), err error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("state: malformed rig id %q", id)
	}
	f, err := os.OpenFile(s.holdPath(id), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("state: hold rig %s: %w", id, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		prev, _ := readHolder(f)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("state: rig %s: %w (pid %d)", id, ErrHeld, prev.PID)
		}
		return nil, fmt.Errorf("state: hold rig %s: %w", id, err)
	}
	if prev, err := readHolder(f); err == nil {
		h.EarlierLogs = prev.Logs()
		if n := len(h.EarlierLogs); n > 0 && h.EarlierLogs[n-1] == h.Log {
			h.EarlierLogs = h.EarlierLogs[:n-1]
		}
		if n := len(h.EarlierLogs); n > maxEarlierLogs {
			h.EarlierLogs = h.EarlierLogs[n-maxEarlierLogs:]
		}
	}
	b, _ := json.Marshal(h)
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt(append(b, '\n'), 0)
		_ = f.Sync()
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// HolderOf reports the rig's holder and whether it holds the rig now. With no
// holder now, the last one's record is returned, if there was one.
func (s *Store) HolderOf(id string) (Holder, bool, error) {
	if !ValidID(id) {
		return Holder{}, false, fmt.Errorf("state: malformed rig id %q", id)
	}
	f, err := os.Open(s.holdPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return Holder{}, false, nil
	}
	if err != nil {
		return Holder{}, false, fmt.Errorf("state: holder of %s: %w", id, err)
	}
	defer f.Close()
	h, _ := readHolder(f)
	// A shared lock is granted only when nobody holds the exclusive one.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return h, true, nil
		}
		return h, false, fmt.Errorf("state: holder of %s: %w", id, err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return h, false, nil
}

func readHolder(f *os.File) (Holder, error) {
	var h Holder
	b, err := io.ReadAll(io.NewSectionReader(f, 0, 1<<16))
	if err != nil {
		return h, err
	}
	if len(b) == 0 {
		return h, nil
	}
	return h, json.Unmarshal(b, &h)
}
