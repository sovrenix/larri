// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package buildinfo

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var semver = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// The version is reported to MCP hosts, quoted in bug reports, and sent to
// providers as a User-Agent. A value that is not a version makes all three
// useless, and the failure is silent.
func TestVersionIsSemantic(t *testing.T) {
	if !semver.MatchString(Version()) {
		t.Fatalf("version %q is not a semantic version", Version())
	}
}

// The source default must be a real release, not a placeholder. `go install`
// and `go run` never see the Makefile, so a "dev" baked into the source is what
// most users would end up reporting.
func TestSourceDefaultIsARelease(t *testing.T) {
	for _, bad := range []string{"dev", "unknown", "none", ""} {
		if Version() == bad {
			t.Fatalf("version is %q; a build without ldflags would report it", bad)
		}
	}
	major, rest, _ := strings.Cut(Version(), ".")
	minor, _, _ := strings.Cut(rest, ".")
	maj, err := strconv.Atoi(major)
	if err != nil {
		t.Fatalf("major %q: %v", major, err)
	}
	min, err := strconv.Atoi(minor)
	if err != nil {
		t.Fatalf("minor %q: %v", minor, err)
	}
	if maj == 0 && min < 9 {
		t.Errorf("version %s is below the 0.9 baseline", Version())
	}
}

// A pre-1.0 build must say so rather than let stability be assumed by
// omission — anyone wiring tools against the CLI or config format is relying
// on this being stated.
func TestPrereleaseTracksTheMajor(t *testing.T) {
	if !Prerelease() {
		t.Error("0.x should report as pre-release")
	}
	old := version
	t.Cleanup(func() { version = old })

	version = "1.0.0"
	if Prerelease() {
		t.Error("1.0.0 should not report as pre-release")
	}
	version = "2.4.1"
	if Prerelease() {
		t.Error("2.4.1 should not report as pre-release")
	}
}

// Commit and Date must degrade to something, never to an empty string that
// renders as a blank in the middle of a version line.
func TestCommitAndDateAreAlwaysPopulated(t *testing.T) {
	if Commit() == "" || Date() == "" {
		t.Errorf("commit=%q date=%q", Commit(), Date())
	}
}

// The ldflags path is the release path, so it has to actually take effect.
func TestInjectedValuesWin(t *testing.T) {
	oldV, oldC, oldD := version, commit, date
	t.Cleanup(func() { version, commit, date = oldV, oldC, oldD })

	version, commit, date = "1.2.3", "abc123def456", "2026-01-02T03:04:05Z"
	s := String()
	for _, want := range []string{"larri 1.2.3", "abc123def456", "2026-01-02T03:04:05Z"} {
		if !strings.Contains(s, want) {
			t.Errorf("%q missing from %q", want, s)
		}
	}
	if UserAgent() != "larri/1.2.3" {
		t.Errorf("user agent = %q", UserAgent())
	}
}

// A dirty tree must be visible in the version. A bug report quoting a clean
// commit that was not the code being run is worse than no commit at all.
func TestDirtyTreeIsMarked(t *testing.T) {
	oldC := commit
	t.Cleanup(func() { commit = oldC })
	commit = "abc123-dirty"
	if !strings.Contains(String(), "-dirty") {
		t.Error("a dirty build did not say so")
	}
}
