// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package lint

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// clawImpls is where claw implementations live.
//
// Under the contract they implement, for the same reason provider adapters live
// under internal/provider: the parent defines the interface, the children
// implement it, and the import direction makes a cycle impossible.
const clawImpls = "go.sovrenix.com/larri/internal/claw/"

// The lifecycle must not learn which application it is carrying.
//
// This is the property the claw layer exists for, and it is the one that decays
// silently. Reaching into an implementation for "just one thing" compiles,
// passes every other test, and works — and the next application then needs the
// same exception, until the daemon knows about all of them and adding one means
// editing the lifecycle. That is the state this replaced: thirteen references
// to a single application inside the daemon, and a bespoke command per type.
//
// internal/claw itself is allowed, and is the only thing that is.
func TestTheDaemonImportsNoClawImplementation(t *testing.T) {
	const pkg = "../daemon"
	entries, err := os.ReadDir(pkg)
	if err != nil {
		t.Skipf("daemon not readable from here: %v", err)
	}

	var checked int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// Tests may reach for a fake implementation: that is how both sites
		// are exercised without spending, and it is not the coupling this
		// guards against.
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(pkg, name)
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		checked++
		for _, imp := range f.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			if strings.HasPrefix(p, clawImpls) {
				t.Errorf("%s imports %s\n"+
					"    the lifecycle must not know which application it is carrying;\n"+
					"    put what it needs behind an interface in internal/claw",
					filepath.ToSlash(path), p)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no daemon sources were inspected; the guard has stopped guarding")
	}
	t.Logf("checked %d daemon sources", checked)
}

// The contract must not depend on the things implementing it.
//
// A cycle is impossible in Go, so this would not compile — but it would be
// "fixed" by moving an implementation detail up into the contract, which is the
// same erosion by a different route.
func TestTheClawContractImportsNoImplementation(t *testing.T) {
	const pkg = "../claw"
	entries, err := os.ReadDir(pkg)
	if err != nil {
		t.Skipf("claw not readable from here: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(pkg, name)
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		for _, imp := range f.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			if strings.HasPrefix(p, clawImpls) {
				t.Errorf("%s imports %s; the contract must not depend on what implements it",
					filepath.ToSlash(path), p)
			}
		}
	}
}
