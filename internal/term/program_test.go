// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package term

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

type stub struct {
	view string
	seen []Msg
}

func (s *stub) Init() Cmd { return nil }
func (s *stub) Update(m Msg) (Model, Cmd) {
	s.seen = append(s.seen, m)
	return s, nil
}
func (s *stub) View() string { return s.view }

// Without a terminal there is nothing to put into raw mode, and trying would
// corrupt a pipe. The caller gets a named error it can act on rather than a
// hang or a mangled stream.
func TestRunWithoutATerminalRefusesCleanly(t *testing.T) {
	r, w, err := osPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	p := NewProgram(&stub{})
	p.in = r
	var out strings.Builder
	p.out = &out

	if _, err := p.Run(); !errors.Is(err, ErrNotATerminal) {
		t.Fatalf("err = %v, want ErrNotATerminal", err)
	}
	// And nothing was written: no alt-screen switch, no escape sequences into
	// what might be a pipe someone is parsing.
	if out.String() != "" {
		t.Errorf("wrote %q to a non-terminal", out.String())
	}
}

// The render path must erase what the previous frame left. Without the
// per-line clear, a shorter line leaves the tail of the longer one behind and
// the dashboard accumulates garbage.
func TestRenderClearsStaleOutput(t *testing.T) {
	var out strings.Builder
	p := &Program{out: &out, model: &stub{view: "hello"}}
	var last string
	p.render(&last)

	got := out.String()
	if !strings.Contains(got, cursorHome) {
		t.Error("does not home the cursor, so frames would scroll")
	}
	if !strings.Contains(got, clearLine) {
		t.Error("does not clear each line, so a shorter frame leaves a tail")
	}
	if !strings.Contains(got, clearBelow) {
		t.Error("does not clear below, so a shorter frame leaves old lines")
	}
	if !strings.Contains(got, "hello") {
		t.Error("the view was not written")
	}
}

// Redrawing an identical frame is the common case — these surfaces tick once a
// second and mostly produce the same string. Writing it again is pure flicker.
func TestRenderSkipsAnUnchangedFrame(t *testing.T) {
	var out strings.Builder
	m := &stub{view: "same"}
	p := &Program{out: &out, model: m}

	var last string
	p.render(&last)
	n := out.Len()
	p.render(&last)
	if out.Len() != n {
		t.Error("redrew an unchanged frame")
	}
	m.view = "different"
	p.render(&last)
	if out.Len() == n {
		t.Error("did not redraw after the view changed")
	}
}

// Send must not block once the program is done, or a background goroutine
// still reporting progress would deadlock against a dashboard the operator
// has already quit.
func TestSendDoesNotBlockAfterQuit(t *testing.T) {
	p := NewProgram(&stub{})
	close(p.done)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			p.Send(SizeMsg{Width: 80, Height: 24})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-timeoutAfter():
		t.Fatal("Send blocked after the program ended")
	}
}

func osPipe() (*os.File, *os.File, error) { return os.Pipe() }

func timeoutAfter() <-chan time.Time { return time.After(3 * time.Second) }
