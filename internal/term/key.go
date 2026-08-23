// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package term

import (
	"unicode/utf8"
)

// KeyType names the keys LARRI's surfaces respond to.
//
// Only those. A terminal can deliver F13, shift-tab and a mouse wheel, and
// decoding all of it would be work in service of nothing — every key not
// listed here arrives as KeyUnknown and is ignored, which is what the surfaces
// did with them anyway.
type KeyType int

const (
	KeyUnknown KeyType = iota
	KeyRunes
	KeyEnter
	KeyEsc
	KeyBackspace
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyCtrlC
	KeySpace
)

// KeyMsg is a keypress.
type KeyMsg struct {
	Type  KeyType
	Runes []rune
}

// String renders a key the way the surfaces match on it.
func (k KeyMsg) String() string {
	switch k.Type {
	case KeyRunes:
		return string(k.Runes)
	case KeyEnter:
		return "enter"
	case KeyEsc:
		return "esc"
	case KeyBackspace:
		return "backspace"
	case KeyUp:
		return "up"
	case KeyDown:
		return "down"
	case KeyLeft:
		return "left"
	case KeyRight:
		return "right"
	case KeyCtrlC:
		return "ctrl+c"
	case KeySpace:
		return " "
	}
	return ""
}

// decode turns a read buffer into keys, returning how many bytes it consumed.
//
// Returns 0 when the buffer holds the start of a sequence but not all of it —
// an escape byte at the end of a read, say. The caller keeps the remainder and
// reads more, which is what stops a chunk boundary in the middle of an arrow
// key from being delivered as a bare Escape.
func decode(b []byte) (KeyMsg, int) {
	if len(b) == 0 {
		return KeyMsg{}, 0
	}
	switch b[0] {
	case 0x03:
		return KeyMsg{Type: KeyCtrlC}, 1
	case 0x0d, 0x0a:
		return KeyMsg{Type: KeyEnter}, 1
	case 0x7f, 0x08:
		return KeyMsg{Type: KeyBackspace}, 1
	case 0x20:
		return KeyMsg{Type: KeySpace, Runes: []rune{' '}}, 1
	case 0x1b:
		// A lone escape is Esc; ESC [ X is a cursor key. Anything else
		// starting with ESC is something we do not use, and is swallowed
		// rather than delivered as a stray Escape that would close a form.
		if len(b) == 1 {
			return KeyMsg{Type: KeyEsc}, 1
		}
		if b[1] != '[' && b[1] != 'O' {
			return KeyMsg{Type: KeyEsc}, 1
		}
		if len(b) < 3 {
			return KeyMsg{}, 0 // incomplete; wait for more
		}
		switch b[2] {
		case 'A':
			return KeyMsg{Type: KeyUp}, 3
		case 'B':
			return KeyMsg{Type: KeyDown}, 3
		case 'C':
			return KeyMsg{Type: KeyRight}, 3
		case 'D':
			return KeyMsg{Type: KeyLeft}, 3
		}
		// A longer CSI sequence: consume through its final byte so its tail
		// is never mistaken for typed text.
		for i := 2; i < len(b); i++ {
			if b[i] >= 0x40 && b[i] <= 0x7e {
				return KeyMsg{Type: KeyUnknown}, i + 1
			}
		}
		return KeyMsg{}, 0
	}
	if b[0] < 0x20 {
		return KeyMsg{Type: KeyUnknown}, 1 // other control bytes
	}
	r, n := utf8.DecodeRune(b)
	if r == utf8.RuneError && n <= 1 {
		if !utf8.FullRune(b) {
			return KeyMsg{}, 0 // partial multi-byte rune
		}
		return KeyMsg{Type: KeyUnknown}, 1
	}
	return KeyMsg{Type: KeyRunes, Runes: []rune{r}}, n
}
