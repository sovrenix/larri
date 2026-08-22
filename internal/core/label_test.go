// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"strings"
	"testing"
	"time"
)

func sampleLabel() Label {
	return Label{
		RigID: "01M0NQ7592JVVWXGFG3YHHMEKS", Version: LabelVersion,
		Model: "Qwen/Qwen2.5-1.5B-Instruct", Served: "qwen", Runtime: RuntimeVLLM,
		LocalPort: 8000, PriceHr: 0.0421,
		CreatedAt: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC),
	}
}

func sealer(t *testing.T) Sealer {
	t.Helper()
	k, err := NewLabelKey()
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewAEADSealer(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The label is the only record that survives losing local state entirely, so
// everything a recovering LARRI needs must round-trip.
func TestSealedLabelRoundTrips(t *testing.T) {
	s := sealer(t)
	in := sampleLabel()
	enc := EncodeLabel(in, DefaultLabelLimit, s)
	if len(enc) > DefaultLabelLimit {
		t.Fatalf("label is %d chars, over the %d limit", len(enc), DefaultLabelLimit)
	}
	out, ok := DecodeLabelWith(enc, s)
	if !ok {
		t.Fatal("should decode")
	}
	if out.RigID != in.RigID || out.Model != in.Model || out.Runtime != in.Runtime {
		t.Errorf("round trip lost detail: %+v", out)
	}
	if out.LocalPort != 8000 || out.PriceHr != 0.0421 {
		t.Errorf("numeric fields lost: port=%d price=%v", out.LocalPort, out.PriceHr)
	}
	if !out.CreatedAt.Equal(in.CreatedAt) {
		t.Errorf("timestamp = %s, want %s", out.CreatedAt, in.CreatedAt)
	}
}

// The detail is hidden from the party the label is stamped in front of.
func TestSealedLabelHidesDetailFromTheHost(t *testing.T) {
	enc := EncodeLabel(sampleLabel(), DefaultLabelLimit, sealer(t))
	for _, secret := range []string{"Qwen", "vllm", "8000", "0.0421"} {
		if strings.Contains(enc, secret) {
			t.Errorf("label leaks %q in the clear: %s", secret, enc)
		}
	}
}

// The deliberate exception: attribution must survive losing the key, because
// an orphan nobody can attribute is worse than one whose details are opaque.
func TestAttributionSurvivesWithoutTheKey(t *testing.T) {
	in := sampleLabel()
	enc := EncodeLabel(in, DefaultLabelLimit, sealer(t))

	out, ok := DecodeLabel(enc) // no key at all
	if !ok {
		t.Fatal("a sealed label must still be recognised as LARRI's")
	}
	if out.RigID != in.RigID {
		t.Fatalf("rig ID = %q, want %q; attribution cannot depend on the key",
			out.RigID, in.RigID)
	}
	if !out.Sealed {
		t.Error("the caller should be told the details are sealed, not left guessing")
	}
	if !strings.Contains(out.Describe(), "sealed") {
		t.Errorf("Describe should say why detail is missing: %s", out.Describe())
	}
}

// A wrong key must not silently yield a plausible-looking label.
func TestWrongKeyDoesNotForgeDetail(t *testing.T) {
	enc := EncodeLabel(sampleLabel(), DefaultLabelLimit, sealer(t))
	out, ok := DecodeLabelWith(enc, sealer(t)) // a different key
	if !ok {
		t.Fatal("still attributable")
	}
	if out.Model != "" || out.LocalPort != 0 {
		t.Errorf("a wrong key produced detail: %+v", out)
	}
	if !out.Sealed {
		t.Error("a failed open must be reported")
	}
}

// Two rigs with identical details must not produce identical labels, or a host
// could correlate them.
func TestSealedLabelsAreNotDeterministic(t *testing.T) {
	s := sealer(t)
	a := EncodeLabel(sampleLabel(), DefaultLabelLimit, s)
	b := EncodeLabel(sampleLabel(), DefaultLabelLimit, s)
	if a == b {
		t.Fatal("identical inputs produced identical labels; the nonce is not doing its job")
	}
}

// Providers cap label length without always saying where, so truncation must
// remove detail rather than attribution.
func TestTruncationKeepsTheRigID(t *testing.T) {
	in := sampleLabel()
	for _, limit := range []int{40, 60, 120} {
		enc := EncodeLabel(in, limit, nil)
		if len(enc) > limit && !strings.HasPrefix(enc, LabelKey+":"+in.RigID) {
			t.Fatalf("limit %d produced %q", limit, enc)
		}
		out, ok := DecodeLabel(enc)
		if !ok || out.RigID != in.RigID {
			t.Fatalf("limit %d lost attribution: %q", limit, enc)
		}
	}
}

// A label written by a later version must remain attributable to this one.
func TestUnknownFieldsAreIgnoredNotRejected(t *testing.T) {
	future := LabelKey + ":01M0NQ7592JVVWXGFG3YHHMEKS|v=99|m=x|zz=something|qq=1"
	out, ok := DecodeLabel(future)
	if !ok {
		t.Fatal("a future label must still be recognised")
	}
	if out.RigID != "01M0NQ7592JVVWXGFG3YHHMEKS" || out.Model != "x" {
		t.Errorf("known fields should still parse: %+v", out)
	}
}

// A model name containing the separators must not be able to forge a field.
func TestSeparatorsInValuesCannotForgeFields(t *testing.T) {
	in := sampleLabel()
	in.Model = "evil|p=1|m=other"
	out, ok := DecodeLabel(EncodeLabel(in, DefaultLabelLimit, nil))
	if !ok {
		t.Fatal("should decode")
	}
	if out.LocalPort == 1 {
		t.Error("a value forged a field")
	}
}

func TestNonLarriLabelsAreNotClaimed(t *testing.T) {
	for _, s := range []string{"", "someone-else", "larri", "larri:", "notlarri:x"} {
		if _, ok := DecodeLabel(s); ok {
			t.Errorf("%q must not be attributed to LARRI", s)
		}
	}
}
