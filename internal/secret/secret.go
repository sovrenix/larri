// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package secret provides a string type whose value cannot be printed by
// accident.
//
// FR-SEC-02 requires secrets to be redacted in logs, TUI output, MCP tool
// results, error messages, and span attributes. That is five code paths, and a
// rule that has to hold at every call site in five code paths is a rule that
// will be broken. Secret makes redaction structural instead: the only way to
// obtain the value is to ask for it by name.
package secret

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
)

// Redacted is what a Secret renders as wherever a string is expected.
const Redacted = "***"

// Secret holds a credential. Its String, GoString, Format, and MarshalJSON
// methods all render Redacted, so a Secret that reaches a log line, a %v, a
// struct dump, or a JSON payload discloses nothing.
type Secret struct {
	v string
}

// New wraps a known value.
func New(v string) Secret { return Secret{v: v} }

// Generate returns a cryptographically random Secret with the given number of
// bytes of entropy, base64url-encoded. FR-SEC-25 requires high-entropy
// credentials; 32 bytes is the project default.
func Generate(nbytes int) (Secret, error) {
	if nbytes <= 0 {
		return Secret{}, fmt.Errorf("secret: entropy must be positive, got %d", nbytes)
	}
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		return Secret{}, fmt.Errorf("secret: reading entropy: %w", err)
	}
	return Secret{v: base64.RawURLEncoding.EncodeToString(b)}, nil
}

// Reveal returns the underlying value. Every call site is a deliberate
// disclosure and should be reviewable by grepping for this method.
func (s Secret) Reveal() string { return s.v }

// Empty reports whether no value is held.
func (s Secret) Empty() bool { return s.v == "" }

// Equal compares in constant time (FR-SEC-25), so verifying a token does not
// leak its prefix through timing.
func (s Secret) Equal(other Secret) bool {
	return subtle.ConstantTimeCompare([]byte(s.v), []byte(other.v)) == 1
}

// String implements fmt.Stringer.
func (s Secret) String() string { return Redacted }

// GoString implements fmt.GoStringer, covering %#v.
func (s Secret) GoString() string { return Redacted }

// Format covers every fmt verb, including %s, %q, %v and %d, so no verb
// reaches the underlying string.
func (s Secret) Format(f fmt.State, verb rune) { fmt.Fprint(f, Redacted) }

// MarshalJSON renders the redaction, so a Secret inside a state snapshot,
// an API response, or a span attribute cannot serialise its value.
func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"` + Redacted + `"`), nil
}

// UnmarshalJSON refuses to load a value back. Secrets are resolved from the
// environment or the OS keyring (FR-SEC-01) and are never persisted, so
// reading one out of JSON means something upstream is wrong.
func (s *Secret) UnmarshalJSON(b []byte) error {
	if string(b) == `"`+Redacted+`"` || string(b) == "null" {
		return nil
	}
	return fmt.Errorf("secret: refusing to unmarshal a credential from JSON; " +
		"secrets come from the environment or keyring (FR-SEC-01)")
}
