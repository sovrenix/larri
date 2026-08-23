// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package term is LARRI's terminal surface: a render loop, key decoding, and
// enough styling for a dashboard.
//
// It exists to replace a dependency tree. The surfaces here used two calls
// from a styling library and a handful of types from an event-loop library,
// and those two brought nineteen of the project's twenty-two modules with them
// — from eight different organisations, for 9% of the binary. For a tool whose
// pitch is that the rented machine is untrusted, that much unaudited
// supply-chain surface for one optional screen was the wrong trade.
//
// What is not here is the reason the trade works: no mouse, no bracketed
// paste, no colour-profile negotiation, no frame diffing. The surfaces never
// used them. If the console pane of §14.4 ever lands and needs any of it, that
// is the moment to reconsider rather than to reimplement.
package term

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

// Msg is anything a Model reacts to.
type Msg any

// Cmd is work that produces a Msg. Nil means nothing to do.
type Cmd func() Msg

// Model is a surface: state, a transition function, and a rendering.
//
// The same shape the previous library used, deliberately — the surfaces were
// written against it, their tests drive Update directly, and there was no
// reason to make them change to save a dependency.
type Model interface {
	Init() Cmd
	Update(Msg) (Model, Cmd)
	View() string
}

// SizeMsg reports the terminal's dimensions.
type SizeMsg struct{ Width, Height int }

// QuitMsg ends the program.
type QuitMsg struct{}

// Quit is a Cmd that ends the program.
func Quit() Msg { return QuitMsg{} }

// Tick schedules a Msg after d.
func Tick(d time.Duration, fn func(time.Time) Msg) Cmd {
	return func() Msg {
		t := time.NewTimer(d)
		defer t.Stop()
		return fn(<-t.C)
	}
}

// ErrNotATerminal is returned when there is no terminal to draw on.
var ErrNotATerminal = errors.New("term: not a terminal")

// Program runs a Model against the terminal.
type Program struct {
	model Model
	in    *os.File
	out   io.Writer

	mu   sync.Mutex
	msgs chan Msg
	done chan struct{}
	once sync.Once

	altScreen bool
	ctx       context.Context
}

// Option configures a Program.
type Option func(*Program)

// WithAltScreen draws on the alternate screen, leaving the scrollback intact.
func WithAltScreen() Option { return func(p *Program) { p.altScreen = true } }

// WithContext ends the program when ctx does.
func WithContext(ctx context.Context) Option { return func(p *Program) { p.ctx = ctx } }

// NewProgram prepares a Model to run.
func NewProgram(m Model, opts ...Option) *Program {
	p := &Program{
		model: m, in: os.Stdin, out: os.Stdout,
		msgs: make(chan Msg, 64), done: make(chan struct{}),
		ctx: context.Background(),
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Send delivers a Msg from outside the loop.
//
// Non-blocking on a closed program, so a background goroutine reporting
// progress cannot deadlock on a dashboard the operator has already quit.
func (p *Program) Send(m Msg) {
	select {
	case <-p.done:
	case p.msgs <- m:
	default:
	}
}

// Run draws until the Model quits, and returns it in its final state.
//
// The terminal is restored on every path out — normal exit, error, panic, or
// signal. Leaving a terminal in raw mode with no echo is the worst failure a
// program like this has, because the shell it hands back appears broken and
// the user has no obvious way to fix it.
func (p *Program) Run() (Model, error) {
	fd := int(p.in.Fd())
	if !term.IsTerminal(fd) {
		return p.model, ErrNotATerminal
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return p.model, fmt.Errorf("term: raw mode: %w", err)
	}
	restore := func() {
		p.once.Do(func() {
			if p.altScreen {
				fmt.Fprint(p.out, exitAltScreen)
			}
			fmt.Fprint(p.out, showCursor)
			_ = term.Restore(fd, state)
		})
	}
	defer restore()

	// A signal that would otherwise kill the process mid-draw restores first.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)

	// And a panic must not leave the terminal raw either.
	defer func() {
		if r := recover(); r != nil {
			restore()
			panic(r)
		}
	}()

	if p.altScreen {
		fmt.Fprint(p.out, enterAltScreen)
	}
	fmt.Fprint(p.out, hideCursor)

	go p.readKeys()
	go p.watchResize(sig)

	if w, h, err := term.GetSize(fd); err == nil {
		p.Send(SizeMsg{Width: w, Height: h})
	}
	if cmd := p.model.Init(); cmd != nil {
		go func() { p.Send(cmd()) }()
	}

	var last string
	p.render(&last)

	for {
		select {
		case <-p.ctx.Done():
			close(p.done)
			return p.model, nil
		case s := <-sig:
			close(p.done)
			restore()
			if s == syscall.SIGINT {
				return p.model, nil
			}
			return p.model, fmt.Errorf("term: %v", s)
		case m := <-p.msgs:
			if _, quit := m.(QuitMsg); quit {
				close(p.done)
				return p.model, nil
			}
			next, cmd := p.model.Update(m)
			p.model = next
			if cmd != nil {
				go func() { p.Send(cmd()) }()
			}
			p.render(&last)
		}
	}
}

// render redraws only when the view changed.
//
// Cheap frame-level diffing rather than the cell-level kind: the surfaces here
// redraw on a one-second tick and mostly produce an identical string, so
// skipping those writes removes nearly all the flicker for none of the
// complexity.
func (p *Program) render(last *string) {
	view := p.model.View()
	if view == *last {
		return
	}
	*last = view

	var b strings.Builder
	b.WriteString(cursorHome)
	for _, line := range strings.Split(view, "\n") {
		b.WriteString(line)
		b.WriteString(clearLine) // erase whatever the previous frame left
		b.WriteString("\r\n")
	}
	b.WriteString(clearBelow)
	fmt.Fprint(p.out, b.String())
}

// readKeys decodes stdin until the program ends.
func (p *Program) readKeys() {
	buf := make([]byte, 0, 256)
	chunk := make([]byte, 256)
	for {
		n, err := p.in.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for len(buf) > 0 {
				k, used := decode(buf)
				if used == 0 {
					break // incomplete sequence; wait for the rest
				}
				buf = buf[used:]
				if k.Type == KeyUnknown {
					continue
				}
				select {
				case <-p.done:
					return
				default:
				}
				p.Send(k)
			}
		}
		if err != nil {
			return
		}
		select {
		case <-p.done:
			return
		default:
		}
	}
}

// watchResize reports terminal size changes.
func (p *Program) watchResize(_ chan os.Signal) {
	win := make(chan os.Signal, 1)
	signal.Notify(win, syscall.SIGWINCH)
	defer signal.Stop(win)
	for {
		select {
		case <-p.done:
			return
		case <-win:
			if w, h, err := term.GetSize(int(p.in.Fd())); err == nil {
				p.Send(SizeMsg{Width: w, Height: h})
			}
		}
	}
}

// The escape sequences this package needs, named rather than inlined so the
// render path reads as intent instead of as punctuation.
const (
	enterAltScreen = "\x1b[?1049h"
	exitAltScreen  = "\x1b[?1049l"
	hideCursor     = "\x1b[?25l"
	showCursor     = "\x1b[?25h"
	cursorHome     = "\x1b[H"
	clearLine      = "\x1b[K"
	clearBelow     = "\x1b[J"
)
