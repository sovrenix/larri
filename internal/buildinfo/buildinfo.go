// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package buildinfo is the one place LARRI's version is written down.
//
// One place, because a version that appears in three files becomes three
// versions the first time someone bumps two of them — and this one is reported
// to an MCP host, stamped on rented resources, and quoted in bug reports, so
// disagreement between them is disagreement about what a user is running.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

// version is the release this source tree represents.
//
// 0.9.0 rather than 1.0.0 deliberately. LARRI rents GPUs, serves models, and
// tears them down with absence confirmed — verified repeatedly against live
// hardware — but a 1.0 would promise stability across a surface that has not
// been tested where it matters most: one provider adapter is not a proven
// abstraction, and whatever RunPod turns out to need will change the interface
// rather than merely add to it. See docs/LARRI_Design_Document.md §20.
//
// Overridden at build time by the Makefile; this value is what `go run` and
// `go install` report, so it must be right in the source rather than only in
// the release pipeline.
var version = "0.9.0"

// commit and date are injected by the build. When they are not — a `go install`
// from source, say — they are recovered from the module's embedded VCS stamp
// instead of reported as "unknown", which tells nobody anything.
var (
	commit = ""
	date   = ""
)

// Version returns the semantic version.
func Version() string { return version }

// Commit returns the source revision, short form, with "-dirty" if the tree
// had uncommitted changes.
func Commit() string {
	if commit != "" {
		return commit
	}
	c, dirty := vcs()
	if c == "" {
		return "unknown"
	}
	if dirty {
		return c + "-dirty"
	}
	return c
}

// Date returns the build date.
func Date() string {
	if date != "" {
		return date
	}
	if _, t := vcsTime(); t != "" {
		return t
	}
	return "unknown"
}

// vcs reads the revision Go embeds in a module built from a repository.
func vcs() (rev string, dirty bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
			if len(rev) > 12 {
				rev = rev[:12]
			}
		case "vcs.modified":
			dirty, _ = strconv.ParseBool(s.Value)
		}
	}
	return rev, dirty
}

func vcsTime() (bool, string) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return false, ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.time" {
			return true, s.Value
		}
	}
	return false, ""
}

// String renders the full build identity, the form `larri version` prints.
func String() string {
	return fmt.Sprintf("larri %s (%s) built %s %s/%s with %s",
		Version(), Commit(), Date(), runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// UserAgent identifies LARRI to a provider.
//
// Providers log these, and a version in the log is what turns "some client is
// hammering the offers endpoint" into a specific release with a specific bug.
func UserAgent() string { return "larri/" + Version() }

// Prerelease reports whether this is a pre-1.0 build.
//
// It exists so surfaces can say so rather than imply stability by omission: a
// version below 1.0 is a statement that the interface may still move, and that
// is worth making out loud to anyone wiring tools against it.
func Prerelease() bool {
	major, _, ok := strings.Cut(Version(), ".")
	if !ok {
		return true
	}
	n, err := strconv.Atoi(major)
	return err != nil || n < 1
}
