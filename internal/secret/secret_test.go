// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package secret

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const canary = "sk-live-DO-NOT-LEAK-4f2a9c"

// The point of the type is that no formatting path reaches the value, so the
// test enumerates the paths rather than checking one.
func TestNoFormattingVerbLeaks(t *testing.T) {
	s := New(canary)
	for _, format := range []string{"%s", "%v", "%q", "%#v", "%+v", "%d", "%x", "%T:%v"} {
		got := fmt.Sprintf(format, s)
		if strings.Contains(got, canary) {
			t.Errorf("verb %s leaked the value: %s", format, got)
		}
	}
}

// A Secret nested in a struct is the realistic case: something logs the whole
// rig, not the credential.
func TestNestedStructDoesNotLeak(t *testing.T) {
	type instance struct {
		Host  string
		Token Secret
	}
	inst := instance{Host: "ssh4.example", Token: New(canary)}

	if got := fmt.Sprintf("%+v", inst); strings.Contains(got, canary) {
		t.Errorf("struct formatting leaked: %s", got)
	}
	b, err := json.Marshal(inst)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), canary) {
		t.Errorf("json leaked: %s", b)
	}
	if !strings.Contains(string(b), Redacted) {
		t.Errorf("json should show the redaction, got %s", b)
	}
}

func TestRevealIsTheOnlyWayOut(t *testing.T) {
	if got := New(canary).Reveal(); got != canary {
		t.Errorf("Reveal() = %q, want the original value", got)
	}
}

func TestUnmarshalRefusesToLoadACredential(t *testing.T) {
	var s Secret
	if err := json.Unmarshal([]byte(`"`+canary+`"`), &s); err == nil {
		t.Fatal("unmarshalling a real value should fail: secrets are not persisted (FR-SEC-01)")
	}
	// Round-tripping our own redaction is fine — that is what reading back a
	// state snapshot looks like.
	if err := json.Unmarshal([]byte(`"`+Redacted+`"`), &s); err != nil {
		t.Errorf("round-tripping the redaction should succeed, got %v", err)
	}
	if !s.Empty() {
		t.Error("a round-tripped redaction must not produce a usable credential")
	}
}

func TestEqual(t *testing.T) {
	a, b := New("same"), New("same")
	if !a.Equal(b) {
		t.Error("identical secrets should compare equal")
	}
	if a.Equal(New("different")) {
		t.Error("differing secrets should not compare equal")
	}
	// Length differences must not panic or short-circuit incorrectly.
	if a.Equal(New("")) {
		t.Error("empty should not equal non-empty")
	}
}

func TestGenerate(t *testing.T) {
	a, err := Generate(32)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	b, err := Generate(32)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if a.Equal(b) {
		t.Error("two generated secrets must differ")
	}
	if len(a.Reveal()) < 40 {
		t.Errorf("32 bytes should encode to >=40 chars, got %d", len(a.Reveal()))
	}
	if _, err := Generate(0); err == nil {
		t.Error("zero entropy must be rejected")
	}
}
