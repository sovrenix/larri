// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sshx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
)

// Session adapts a Client to runtime.Session, so the runtime layer can drive a
// host without knowing how it is reached.
type Session struct{ c *Client }

var _ runtime.Session = (*Session)(nil)

// Session opens a runtime session over this connection. It is a view, not a
// new connection: exec, log streaming, and metrics collection all share the
// one handshake (§17.4).
func (c *Client) Session() *Session { return &Session{c: c} }

// Run executes a command and returns its combined output.
func (s *Session) Run(ctx context.Context, cmd string) ([]byte, error) {
	sess, err := s.c.ssh.NewSession()
	if err != nil {
		return nil, errs.New(errs.ClassHostFailure, "sshx.Run", err)
	}
	defer sess.Close()

	// One buffer, but two writers: x/crypto/ssh copies stdout and stderr on
	// separate goroutines, and bytes.Buffer is not safe for concurrent use.
	// Sharing one directly loses writes intermittently — which would have
	// meant silently dropping the stderr of a failed bootstrap command, the
	// one output an operator needs from a host they cannot casually log into.
	var buf syncBuffer
	sess.Stdout = &buf
	sess.Stderr = &buf

	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()

	select {
	case <-ctx.Done():
		_ = sess.Signal("KILL")
		return buf.Bytes(), ctx.Err()
	case err := <-done:
		if err != nil {
			// Output is returned alongside the error: a failed command's
			// stderr is usually the only useful diagnosis available from a
			// host the operator cannot log into casually.
			return buf.Bytes(), errs.Newf(errs.ClassHostFailure, "sshx.Run",
				"%s: %v", firstLine(buf.Bytes()), err)
		}
		return buf.Bytes(), nil
	}
}

// Stream executes a command and returns its output as it arrives, for a
// weight download or a log tail that must not look like a hang.
func (s *Session) Stream(ctx context.Context, cmd string) (io.ReadCloser, error) {
	sess, err := s.c.ssh.NewSession()
	if err != nil {
		return nil, errs.New(errs.ClassHostFailure, "sshx.Stream", err)
	}
	pr, pw := io.Pipe()
	sess.Stdout = pw
	sess.Stderr = pw
	if err := sess.Start(cmd); err != nil {
		sess.Close()
		_ = pw.CloseWithError(err)
		return nil, errs.New(errs.ClassHostFailure, "sshx.Stream", err)
	}
	go func() {
		err := sess.Wait()
		sess.Close()
		_ = pw.CloseWithError(err)
	}()
	go func() {
		<-ctx.Done()
		_ = sess.Signal("KILL")
	}()
	return pr, nil
}

// Dial opens a channel to a port on the host's loopback interface.
func (s *Session) Dial(_ context.Context, port int) (io.ReadWriteCloser, error) {
	conn, err := s.c.ssh.Dial("tcp", net.JoinHostPort(Loopback, fmt.Sprint(port)))
	if err != nil {
		return nil, errs.New(errs.ClassHostFailure, "sshx.Dial", err)
	}
	return conn, nil
}

// Close releases the session's view. The underlying connection belongs to the
// Client and is closed there.
func (s *Session) Close() error { return nil }

// syncBuffer serialises concurrent writes so stdout and stderr can share one
// destination without racing.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Copy: the caller keeps this after the lock is released, and the ssh
	// copy goroutines may still be running if the context was cancelled.
	return append([]byte(nil), s.buf.Bytes()...)
}

func firstLine(b []byte) string {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	}
	if len(b) > 200 {
		b = b[:200]
	}
	return string(bytes.TrimSpace(b))
}
