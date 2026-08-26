// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Command sitecontent extracts the editable page out of the generated site
// bundle, and puts it back.
//
// gh-pages/index.html is produced by a design tool. Almost all of it is
// machine-written: a 200 KB manifest of base64 assets, a loader, a component
// runtime. Exactly one block is the page an author cares about — a
// JSON-encoded string in <script type="__bundler/template"> holding the
// markup and the content arrays.
//
// Editing that string in place means writing / for every closing slash
// and \" for every attribute, which is how a one-word change becomes a broken
// site. So: extract to real HTML, edit that, embed it back.
//
//	go run ./internal/devtools/sitecontent extract   # -> gh-pages/content.html
//	$EDITOR gh-pages/content.html
//	go run ./internal/devtools/sitecontent embed     # -> gh-pages/index.html
//
// Embed re-decodes what it wrote and compares it to what it was given, and
// refuses to save if they differ. That is the property worth having; byte
// identity is not available, because Go escapes < > and & where the original
// generator escaped / instead. Both decode to the same string, which is what
// the loader reads.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	bundlePath  = "gh-pages/index.html"
	contentPath = "gh-pages/content.html"
	openTag     = `<script type="__bundler/template">`
	closeTag    = `</script>`
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sitecontent extract|embed")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "extract":
		err = extract()
	case "embed":
		err = embed()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sitecontent:", err)
		os.Exit(1)
	}
}

// span locates the JSON string between the template tags.
func span(bundle string) (start, end int, err error) {
	i := strings.Index(bundle, openTag)
	if i < 0 {
		return 0, 0, fmt.Errorf("%s: no %s block", bundlePath, openTag)
	}
	start = i + len(openTag)
	j := strings.Index(bundle[start:], closeTag)
	if j < 0 {
		return 0, 0, fmt.Errorf("%s: unterminated template block", bundlePath)
	}
	return start, start + j, nil
}

func extract() error {
	b, err := os.ReadFile(bundlePath)
	if err != nil {
		return err
	}
	start, end, err := span(string(b))
	if err != nil {
		return err
	}
	var html string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b)[start:end])), &html); err != nil {
		return fmt.Errorf("decode template: %w", err)
	}
	if err := os.WriteFile(contentPath, []byte(html), 0o644); err != nil {
		return err
	}
	fmt.Printf("%s -> %s (%d bytes)\n", bundlePath, contentPath, len(html))
	fmt.Println("edit it as ordinary HTML, then: go run ./internal/devtools/sitecontent embed")
	return nil
}

func embed() error {
	html, err := os.ReadFile(contentPath)
	if err != nil {
		return fmt.Errorf("%w — run extract first", err)
	}
	b, err := os.ReadFile(bundlePath)
	if err != nil {
		return err
	}
	start, end, err := span(string(b))
	if err != nil {
		return err
	}
	// Encoded rather than hand-escaped: getting this right by hand is the
	// whole reason the tool exists.
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(string(html)); err != nil {
		return err
	}
	encoded := strings.TrimRight(buf.String(), "\n")

	// The one escape JSON does not require and this file cannot do without.
	//
	// The template is a JSON string living inside a <script> element, and an
	// HTML parser ends that element at the first literal "</script>" it sees
	// — inside a quoted string or not, because it is not parsing JSON. The
	// content contains one, so leaving it literal truncates the bundle and
	// serves a blank page. The generator escaped every slash for this reason;
	// escaping the sequence that matters is enough and stays readable.
	encoded = strings.ReplaceAll(encoded, "</", `<\/`)

	// Prove the round trip before overwriting anything. A template that does
	// not decode back to what the author wrote is a silently broken site, and
	// this is the last moment it can be caught for free.
	var check string
	if err := json.Unmarshal([]byte(encoded), &check); err != nil {
		return fmt.Errorf("re-encoded template does not parse: %w", err)
	}
	if check != string(html) {
		return fmt.Errorf("round trip altered the content: %s not rewritten", bundlePath)
	}

	out := string(b)[:start] + "\n" + encoded + "\n  " + string(b)[end:]
	if err := os.WriteFile(bundlePath, []byte(out), 0o644); err != nil {
		return err
	}
	fmt.Printf("%s -> %s (%d bytes of content)\n", contentPath, bundlePath, len(html))
	return nil
}
