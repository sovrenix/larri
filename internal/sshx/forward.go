// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sshx

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"go.sovrenix.com/larri/internal/errs"
)

// Loopback is the only address a forward may bind, at either end.
const Loopback = "127.0.0.1"

// Forward carries a local loopback port to a port on the host's loopback.
//
// The remote end is loopback by design: the runtime binds there and no
// provider port mapping exists for it, so this channel is the only way to
// reach it (FR-SEC-08, FR-SEC-15).
type Forward struct {
	ln         net.Listener
	client     *Client
	remotePort int

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
}

// Listen binds the local port and returns a Forward that is not yet carrying
// traffic. Call Serve to start.
//
// **Binding happens here, before the rig is declared healthy**, and that
// ordering is the structural replacement for ExitOnForwardFailure. The likely
// trigger is mundane — the port is still held by a previous run — and the
// failure mode is not: an ssh binary would connect, silently fail to forward,
// and leave a supervisor watching a live process while every client got
// connection refused. Here it is an error returned from net.Listen.
func (c *Client) Listen(localPort, remotePort int) (*Forward, error) {
	addr := net.JoinHostPort(Loopback, fmt.Sprint(localPort))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, errs.Newf(errs.ClassHostFailure, "sshx.Listen",
			"bind %s: %v", addr, err)
	}
	return &Forward{
		ln: ln, client: c, remotePort: remotePort,
		conns: make(map[net.Conn]struct{}),
	}, nil
}

// LocalAddr reports the bound address, which is useful when port 0 was
// requested and the kernel chose.
func (f *Forward) LocalAddr() net.Addr { return f.ln.Addr() }

// LocalPort reports the bound port.
func (f *Forward) LocalPort() int {
	if a, ok := f.ln.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}

// Serve accepts local connections and pipes each to the remote port. It blocks
// until the context is cancelled or the listener is closed.
func (f *Forward) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		f.Close()
	}()
	for {
		local, err := f.ln.Accept()
		if err != nil {
			f.mu.Lock()
			closed := f.closed
			f.mu.Unlock()
			if closed || ctx.Err() != nil {
				return nil
			}
			return errs.New(errs.ClassHostFailure, "sshx.Serve", err)
		}
		f.track(local)
		go f.pipe(ctx, local)
	}
}

func (f *Forward) track(c net.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conns[c] = struct{}{}
}

func (f *Forward) untrack(c net.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.conns, c)
}

func (f *Forward) pipe(ctx context.Context, local net.Conn) {
	defer func() {
		local.Close()
		f.untrack(local)
	}()
	remote, err := f.client.ssh.Dial("tcp",
		net.JoinHostPort(Loopback, fmt.Sprint(f.remotePort)))
	if err != nil {
		return
	}
	defer remote.Close()

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
	go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Close stops accepting and drops every carried connection.
func (f *Forward) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	conns := make([]net.Conn, 0, len(f.conns))
	for c := range f.conns {
		conns = append(conns, c)
	}
	f.mu.Unlock()

	err := f.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	return err
}
