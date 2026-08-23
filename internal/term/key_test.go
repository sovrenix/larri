// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package term

import "testing"

func TestDecodeKeys(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
		used int
	}{
		{"enter (CR)", []byte{0x0d}, "enter", 1},
		{"enter (LF)", []byte{0x0a}, "enter", 1},
		{"backspace (DEL)", []byte{0x7f}, "backspace", 1},
		{"backspace (BS)", []byte{0x08}, "backspace", 1},
		{"ctrl-c", []byte{0x03}, "ctrl+c", 1},
		{"space", []byte{0x20}, " ", 1},
		{"letter", []byte("q"), "q", 1},
		{"lone escape", []byte{0x1b}, "esc", 1},
		{"up", []byte{0x1b, '[', 'A'}, "up", 3},
		{"down", []byte{0x1b, '[', 'B'}, "down", 3},
		{"right", []byte{0x1b, '[', 'C'}, "right", 3},
		{"left", []byte{0x1b, '[', 'D'}, "left", 3},
		{"application-mode up", []byte{0x1b, 'O', 'A'}, "up", 3},
		// A multi-byte rune must arrive whole, not as two mystery bytes.
		{"utf-8 rune", []byte("é"), "é", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k, used := decode(c.in)
			if got := k.String(); got != c.want {
				t.Errorf("key = %q, want %q", got, c.want)
			}
			if used != c.used {
				t.Errorf("consumed %d bytes, want %d", used, c.used)
			}
		})
	}
}

// A read can end in the middle of an escape sequence. Delivering what has
// arrived so far would turn half an arrow key into a bare Escape — which in
// the editor closes the field being typed into.
func TestPartialSequenceWaitsForTheRest(t *testing.T) {
	for _, partial := range [][]byte{
		{0x1b, '['},           // arrow key, cut after the bracket
		{0xc3},                // first byte of a two-byte rune
		{0x1b, '[', '1', ';'}, // a longer CSI, cut mid-parameter
	} {
		if _, used := decode(partial); used != 0 {
			t.Errorf("decode(%v) consumed %d bytes; an incomplete sequence must wait", partial, used)
		}
	}
	// And once the rest arrives it decodes as one key.
	k, used := decode([]byte{0x1b, '[', 'A'})
	if k.String() != "up" || used != 3 {
		t.Errorf("completed sequence = %q/%d", k.String(), used)
	}
}

// An unrecognised CSI sequence must be swallowed whole. Leaving its tail in
// the buffer would deliver "[", "2", "0", "0", "~" as typed characters — which
// is what a bracketed paste looks like when nobody consumes it.
func TestUnknownSequenceIsConsumedEntirely(t *testing.T) {
	in := append([]byte{0x1b, '[', '2', '0', '0', '~'}, 'x')
	k, used := decode(in)
	if k.Type != KeyUnknown {
		t.Errorf("type = %v, want unknown", k.Type)
	}
	if used != 6 {
		t.Fatalf("consumed %d, want the whole 6-byte sequence", used)
	}
	next, _ := decode(in[used:])
	if next.String() != "x" {
		t.Errorf("the following key decoded as %q", next.String())
	}
}

// Keys the surfaces never use must not arrive as anything actionable.
func TestUnusedControlBytesAreIgnored(t *testing.T) {
	for _, b := range []byte{0x01, 0x04, 0x09, 0x1a} {
		k, used := decode([]byte{b})
		if k.Type != KeyUnknown || used != 1 {
			t.Errorf("byte %#x decoded as %v", b, k.Type)
		}
	}
}
