// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package clientkeys

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The value is shown once and stored only as a hash: nothing on disk can be
// turned back into a key a client could present.
func TestAKeyIsStoredOnlyAsAHash(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	key, err := s.Create("continue")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), key.Reveal()) {
		t.Fatal("the key's value was written to disk")
	}
	if !strings.Contains(string(raw), Hash(key.Reveal())) {
		t.Error("the key's hash is not on disk; nothing could match it")
	}
	fi, _ := os.Stat(filepath.Join(dir, FileName))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %o, want 600", fi.Mode().Perm())
	}
	if name, ok := s.Match(key.Reveal()); !ok || name != "continue" {
		t.Errorf("Match = %q, %v", name, ok)
	}
	if _, ok := s.Match(key.Reveal() + "x"); ok {
		t.Error("matched a value that is not the key")
	}
}

// Keys are named for the client they are given to, so one can be revoked
// without rewiring the rest (FR-SEC-23).
func TestNamesAreValidatedAndUnique(t *testing.T) {
	s := Open(t.TempDir())
	if _, err := s.Create("librechat"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("librechat"); !errors.Is(err, ErrExists) {
		t.Errorf("err = %v; a second key under one name would make revoking it ambiguous", err)
	}
	for _, bad := range []string{"", "Upper", "has space", "-leading", strings.Repeat("a", 33), "../x"} {
		if _, err := s.Create(bad); err == nil {
			t.Errorf("accepted name %q", bad)
		}
	}
}

// Revoking a leaked key must work on a rig that is already serving: the
// process holding it re-reads the file, rather than keeping the key accepted
// until the next bring-up.
func TestARevokedKeyStopsMatchingWithoutARestart(t *testing.T) {
	dir := t.TempDir()
	holder := Open(dir) // the process serving the rig
	cli := Open(dir)    // `larri token revoke` in another terminal
	key, err := cli.Create("leaked")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := holder.Match(key.Reveal()); !ok {
		t.Fatal("a created key was not accepted")
	}
	time.Sleep(10 * time.Millisecond) // a distinct modification time
	if err := cli.Revoke("leaked"); err != nil {
		t.Fatal(err)
	}
	if _, ok := holder.Match(key.Reveal()); ok {
		t.Error("a revoked key was still accepted by the process holding the rig")
	}
	if err := cli.Revoke("leaked"); !errors.Is(err, ErrUnknown) {
		t.Errorf("revoking twice: err = %v", err)
	}
}

// A key check that cannot run admits no one.
func TestAnUnreadableKeyFileMatchesNothing(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	key, err := s.Create("default")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Match(key.Reveal()); ok {
		t.Error("a corrupt key file still admitted a key")
	}
	if _, err := s.List(); err == nil {
		t.Error("listing a corrupt key file reported nothing wrong")
	}
}
