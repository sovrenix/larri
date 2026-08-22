// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package state

import (
	"sort"
	"testing"
	"time"
)

func TestIDShape(t *testing.T) {
	id, err := NewID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != IDLen {
		t.Errorf("len = %d, want %d (%q)", len(id), IDLen, id)
	}
	if !ValidID(id) {
		t.Errorf("%q should be valid", id)
	}
	// Crockford excludes these so a transcribed ID cannot become a different
	// valid one.
	for _, bad := range []string{"I", "L", "O", "U"} {
		if ValidID(bad + id[1:]) {
			t.Errorf("%s must not be a valid Crockford digit", bad)
		}
	}
}

// The timestamp prefix is what makes a directory listing chronological and the
// journal read in order without parsing.
func TestIDsSortChronologically(t *testing.T) {
	base := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	var ids []string
	for i := 0; i < 20; i++ {
		id, err := NewID(base.Add(time.Duration(i) * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("IDs must sort lexically by time; position %d differs", i)
		}
	}
}

func TestIDTimeRoundTrip(t *testing.T) {
	want := time.Date(2026, 8, 22, 14, 31, 2, 0, time.UTC)
	id, err := NewID(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := IDTime(id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Errorf("IDTime = %s, want %s", got, want)
	}
}

func TestIDsAreUnique(t *testing.T) {
	now := time.Now()
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id, err := NewID(now) // same millisecond: only entropy differs
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("duplicate ID %q within one millisecond", id)
		}
		seen[id] = true
	}
}

// A malformed ID must never reach the filesystem as a path component.
func TestValidIDRejectsPathTricks(t *testing.T) {
	for _, bad := range []string{
		"", "short", "../../etc/passwd",
		"01J9Z/../../../etc", "01J9ZABCDEFGHJKMNPQRSTVWX.", "01j9zabcdefghjkmnpqrstvwxy",
	} {
		if ValidID(bad) {
			t.Errorf("%q must be rejected", bad)
		}
	}
}
