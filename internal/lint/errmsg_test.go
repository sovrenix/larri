// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package lint enforces conventions that are otherwise only remembered.
package lint

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// Error messages are terse and program-like: `package: subject: problem`,
// lowercase, no trailing punctuation, no conversational asides.
//
// The reasoning behind a refusal belongs in the doc comment, where a reader
// who wants it will look. An error string is read by someone whose command
// just failed, and prose gets in their way. This test enforces that, because
// a convention nobody checks decays into a convention nobody follows.
//
// Informational output — progress, status, disclosures — is exempt. Those are
// read by someone who is not currently stuck, and may be conversational.
func TestErrorMessagesAreTerse(t *testing.T) {
	root := ".."

	// Words that signal an error string is explaining rather than reporting.
	conversational := []string{
		"because", "refusing to", "refuses to", "please", "sorry",
		"we ", "you should", "you must", "note that", "keep in mind",
		"which is", "which means", "so that", "in order to",
	}

	var checked int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			idx, ok := errorFormatArg(call)
			if !ok || idx >= len(call.Args) {
				return true
			}
			lit, msg, ok := stringLit(call.Args[idx])
			if !ok {
				return true
			}
			checked++
			pos := fset.Position(lit.Pos())
			where := func() string {
				return filepath.ToSlash(path) + ":" + strconv.Itoa(pos.Line)
			}

			if r := []rune(msg); len(r) > 0 && unicode.IsUpper(r[0]) {
				t.Errorf("%s: error message starts with a capital: %q\n"+
					"    errors compose when wrapped; a capital mid-chain reads wrong", where(), msg)
			}
			if trimmed := strings.TrimRight(msg, " "); strings.HasSuffix(trimmed, ".") ||
				strings.HasSuffix(trimmed, "!") || strings.HasSuffix(trimmed, "\n") {
				t.Errorf("%s: error message ends with punctuation: %q\n"+
					"    a wrapped error becomes \"outer: inner.: more\"", where(), msg)
			}
			low := strings.ToLower(msg)
			for _, w := range conversational {
				if strings.Contains(low, w) {
					t.Errorf("%s: error message reads conversationally (%q): %q\n"+
						"    state the fact; the reasoning belongs in the doc comment",
						where(), strings.TrimSpace(w), msg)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no error messages were inspected; the walker is broken, not the code")
	}
	t.Logf("checked %d error messages", checked)
}

// errorFormatArg reports the index of the format/message argument for calls
// that construct errors, and whether this is such a call.
func errorFormatArg(call *ast.CallExpr) (int, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return 0, false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return 0, false
	}
	switch pkg.Name + "." + sel.Sel.Name {
	case "fmt.Errorf", "errors.New":
		return 0, true
	case "errs.Newf":
		return 2, true // (class, op, format, ...)
	case "errs.New":
		return 2, true // (class, op, err) — skipped below unless a literal
	}
	return 0, false
}

func stringLit(e ast.Expr) (*ast.BasicLit, string, bool) {
	// Unwrap `"a" + "b"` concatenations, taking the leftmost literal, which
	// is where a leading capital would appear.
	for {
		bin, ok := e.(*ast.BinaryExpr)
		if !ok || bin.Op != token.ADD {
			break
		}
		e = bin.X
	}
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return nil, "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return nil, "", false
	}
	return lit, s, true
}
