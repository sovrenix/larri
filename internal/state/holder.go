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

// The lock and the record are separate files. The lock is only ever locked;
// the record is replaced whole, by rename, so a reader sees the previous
// holder's record or the new one's and never a file half rewritten — which,
// when the record was the lock file's own contents, could name pid 0 as the
// holder at the moment an operator needed to know which process to stop.
func (s *Store) holdPath(id string) string {
	return filepath.Join(s.dir, "rigs", "."+id+".hold")
}

func (s *Store) holderPath(id string) string {
	return filepath.Join(s.dir, "rigs", "."+id+".holder.json")
}

// Hold takes the rig for this process. It is held until release is called or
// the process ends: the lock is the kernel's, so a holder that crashes or is
// killed frees the rig without anyone cleaning up after it.
//
// The record stays after release, so the last holder's log can still be found.
// A record that cannot be written fails the hold: a process serving a rig that
// status, resume and logs cannot attribute to it is the confusion the record
// exists to prevent.
func (s *Store) Hold(id string, h Holder) (release func(), err error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("state: malformed rig id %q", id)
	}
	f, err := os.OpenFile(s.holdPath(id), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("state: hold rig %s: %w", id, err)
	}
	if err := lockExclusive(f); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			prev, _, _ := s.HolderOf(id)
			return nil, fmt.Errorf("state: rig %s: %w (pid %d)", id, ErrHeld, prev.PID)
		}
		return nil, fmt.Errorf("state: hold rig %s: %w", id, err)
	}
	unlock := func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	// Under the lock, the record on disk is the previous holder's and cannot
	// change: its logs become this one's history.
	if prev, err := s.readHolderRecord(id); err == nil {
		h.EarlierLogs = prev.Logs()
		if n := len(h.EarlierLogs); n > 0 && h.EarlierLogs[n-1] == h.Log {
			h.EarlierLogs = h.EarlierLogs[:n-1]
		}
		if n := len(h.EarlierLogs); n > maxEarlierLogs {
			h.EarlierLogs = h.EarlierLogs[n-maxEarlierLogs:]
		}
	}
	if err := s.writeHolderRecord(id, h); err != nil {
		unlock()
		return nil, fmt.Errorf("state: hold rig %s: %w", id, err)
	}
	return unlock, nil
}

// lockExclusive takes the rig's lock without waiting on a holder, but waits
// out a reader. HolderOf probes by taking the lock shared for an instant, and a
// probe at the moment a rig changes hands — `larri logs -f` polls, `larri
// status` runs — used to make `larri resume` fail as though a process held the
// rig. A probe lasts microseconds and a holder lasts as long as the rig, so a
// tenth of a second tells them apart.
func lockExclusive(f *os.File) error {
	deadline := time.Now().Add(100 * time.Millisecond)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// HolderOf reports the rig's holder and whether it holds the rig now. With no
// holder now, the last one's record is returned, if there was one.
func (s *Store) HolderOf(id string) (Holder, bool, error) {
	if !ValidID(id) {
		return Holder{}, false, fmt.Errorf("state: malformed rig id %q", id)
	}
	// A new holder takes the lock and then publishes its record, so for an
	// instant the lock is held under the previous holder's record — or under
	// none. A record naming no live process while the lock is held is that
	// instant, and is read again rather than reported.
	deadline := time.Now().Add(250 * time.Millisecond)
	for {
		held, err := s.lockHeld(id)
		if err != nil {
			return Holder{}, false, err
		}
		h, rerr := s.readHolderRecord(id)
		if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return h, held, fmt.Errorf("state: holder of %s: %w", id, rerr)
		}
		if !held || processAlive(h.PID) || time.Now().After(deadline) {
			return h, held, nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// lockHeld probes the rig's lock. A shared lock is granted only when nobody
// holds the exclusive one.
func (s *Store) lockHeld(id string) (bool, error) {
	f, err := os.Open(s.holdPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("state: holder of %s: %w", id, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return false, fmt.Errorf("state: holder of %s: %w", id, err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}

// readHolderRecord reads the published record. Records written before the
// lock and the record were separated lived in the lock file, and are read from
// there when there is no record of the new kind.
func (s *Store) readHolderRecord(id string) (Holder, error) {
	var h Holder
	b, err := os.ReadFile(s.holderPath(id))
	if errors.Is(err, os.ErrNotExist) {
		b, err = readUpTo(s.holdPath(id), 1<<16)
	}
	if err != nil {
		return h, err
	}
	if len(b) == 0 {
		return h, nil
	}
	return h, json.Unmarshal(b, &h)
}

// writeHolderRecord publishes a record whole: written beside the old one,
// synced, and renamed over it.
func (s *Store) writeHolderRecord(id string, h Holder) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.holderPath(id)), ".holder-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // a no-op once renamed
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.holderPath(id))
}

func readUpTo(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, n))
}

// processAlive reports whether a process with this pid exists. A pid of 0 is
// no process: it is what an unpublished or unreadable record names.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
