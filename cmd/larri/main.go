// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Command larri rents a GPU, serves a model on it, points local tools at it,
// and stops paying when told to.
//
// See docs/LARRI_Requirements_Specification.md and
// docs/LARRI_Design_Document.md. The lifecycle subcommands land in M1; this
// entrypoint currently reports build metadata and the implementation status.
package main

import (
	"fmt"
	"os"
	"runtime"
)

// Stamped at build time via -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `larri — Local Agent for Remote Rigging of Inference

  larri version     build metadata

Lifecycle subcommands (up, down, status, offers, orphans, daemon, mcp, ui)
land in milestone M1. See docs/LARRI_Design_Document.md §20.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Printf("larri %s (%s) built %s %s/%s with %s\n",
			version, commit, date, runtime.GOOS, runtime.GOARCH, runtime.Version())
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "larri: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
