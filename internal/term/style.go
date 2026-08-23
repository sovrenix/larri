// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package term

import (
	"os"
	"strconv"
	"strings"
)

// Style renders text with SGR attributes.
//
// Deliberately small: LARRI's terminal surfaces used exactly two things from
// the library this replaces — a foreground colour and bold — so that is what
// exists here. A styling package that grew borders and layout engines nobody
// called would be the bloat this replaced.
type Style struct {
	fg   string // 256-colour index, empty for default
	bold bool
}

// NewStyle returns an unstyled Style.
func NewStyle() Style { return Style{} }

// Foreground sets a 256-colour index.
func (s Style) Foreground(c Color) Style { s.fg = string(c); return s }

// Bold sets the bold attribute.
func (s Style) Bold(b bool) Style { s.bold = b; return s }

// Render wraps text in the style's escape sequences.
//
// A reset always follows, so a style can never leak into whatever is printed
// next — the failure that leaves a terminal permanently green after a crash.
func (s Style) Render(text string) string {
	if !colorEnabled() || (s.fg == "" && !s.bold) {
		return text
	}
	var b strings.Builder
	b.WriteString("\x1b[")
	first := true
	if s.bold {
		b.WriteString("1")
		first = false
	}
	if s.fg != "" {
		if !first {
			b.WriteByte(';')
		}
		b.WriteString("38;5;")
		b.WriteString(s.fg)
	}
	b.WriteString("m")
	b.WriteString(text)
	b.WriteString("\x1b[0m")
	return b.String()
}

// Color is a 256-colour palette index.
type Color string

// Adaptive picks between a light-background and a dark-background colour.
//
// The library this replaces queried the terminal for its background with an
// OSC 11 request and parsed the reply. That is a round-trip to the terminal on
// startup, a timeout to get wrong, and a parser to maintain — for a decision
// that COLORFGBG already answers on the terminals that set it, and that a
// dark-background default answers acceptably everywhere else.
//
// Being wrong here costs contrast, not correctness, which is why the cheap
// answer is the right one.
func Adaptive(light, dark Color) Color {
	if lightBackground() {
		return light
	}
	return dark
}

// lightBackground reports whether the terminal looks light.
//
// COLORFGBG is "fg;bg" with bg as a colour index; 7 and 15 are the light greys
// terminals use for a light theme. Absent, assume dark: it is by far the more
// common setting, and the palette below is chosen to stay legible either way.
func lightBackground() bool {
	v := os.Getenv("COLORFGBG")
	if v == "" {
		return false
	}
	parts := strings.Split(v, ";")
	bg, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return false
	}
	return bg >= 7 && bg != 8
}

// colorEnabled honours NO_COLOR and a dumb terminal.
//
// https://no-color.org: any value, including empty, disables colour. Piping
// LARRI's output into a file or a log collector should not fill it with escape
// sequences.
func colorEnabled() bool {
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return true
}
