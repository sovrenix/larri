// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package notice

import (
	"strings"
	"testing"
)

// The headline states who can read the data, not merely that a risk exists.
// "Be careful with sensitive data" is advice; naming the reader is a fact the
// operator can act on.
func TestHeadlineNamesTheReader(t *testing.T) {
	h := strings.ToLower(PrivacyHeadline)
	for _, want := range []string{"host operator", "read", "prompts"} {
		if !strings.Contains(h, want) {
			t.Errorf("headline should mention %q: %q", want, PrivacyHeadline)
		}
	}
}

// The full form explains the mechanism, because an operator who understands
// why encryption does not help makes better decisions than one handed a rule.
func TestFullNoticeExplainsWhyEncryptionDoesNotHelp(t *testing.T) {
	f := strings.ToLower(PrivacyFull())
	for _, want := range []string{"root", "plaintext", "confidential computing", "tunnel"} {
		if !strings.Contains(f, want) {
			t.Errorf("full notice should cover %q", want)
		}
	}
	if !strings.Contains(f, "wrong tool") {
		t.Error("it should say plainly when LARRI is not suitable, rather than imply a setting could fix it")
	}
}

// The comment lands in a file the operator reads long after `up` scrolled
// past, where a loopback endpoint looks indistinguishable from a local model.
func TestConfigCommentUsesTheGivenPrefix(t *testing.T) {
	got := ConfigComment("#")
	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "# ") {
			t.Errorf("line not commented: %q", line)
		}
	}
	if !strings.Contains(got, "not a local model") {
		t.Error("the comment must break the illusion that 127.0.0.1 means local")
	}
	if !strings.Contains(got, "larri down") {
		t.Error("the comment should say how to remove the configuration it wrote")
	}
	if !strings.Contains(ConfigComment("//"), "// Configured by LARRI") {
		t.Error("prefix should be configurable for other comment syntaxes")
	}
}

func TestHostSummaryNamesThePlace(t *testing.T) {
	s := HostSummary("vastai", "14872213", "US, California")
	for _, want := range []string{"vastai", "14872213", "US, California"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary should name %q: %s", want, s)
		}
	}
	// A missing region must not render as an empty parenthesis.
	if strings.Contains(HostSummary("vastai", "1", ""), "()") {
		t.Error("absent location should be stated, not left blank")
	}
}
