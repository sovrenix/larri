// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testServer is a real SSH server, so the security properties are verified
// against an actual handshake rather than asserted against a config struct.
type testServer struct {
	Addr    net.Addr
	HostKey ssh.PublicKey

	// PushChannel makes the server open a channel toward the client after
	// authentication, simulating a compromised host reaching back.
	PushChannel bool

	// AllowPassword advertises password authentication, so a client that
	// would fall back to it can be caught doing so.
	AllowPassword bool

	mu       sync.Mutex
	pushErr  error
	pushDone chan struct{}
	ln       net.Listener
}

func newTestServer(t *testing.T, cfg func(*testServer)) *testServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testServer{Addr: ln.Addr(), HostKey: signer.PublicKey(), ln: ln,
		pushDone: make(chan struct{})}
	if cfg != nil {
		cfg(s)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(t, conn, signer)
		}
	}()
	return s
}

func (s *testServer) Port() int { return s.Addr.(*net.TCPAddr).Port }

func (s *testServer) handle(t *testing.T, nConn net.Conn, signer ssh.Signer) {
	conf := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	if s.AllowPassword {
		conf.PasswordCallback = func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		}
	}
	conf.AddHostKey(signer)

	sc, chans, reqs, err := ssh.NewServerConn(nConn, conf)
	if err != nil {
		nConn.Close()
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)

	if s.PushChannel {
		go func() {
			_, _, err := sc.OpenChannel("auth-agent@openssh.com", nil)
			s.mu.Lock()
			s.pushErr = err
			s.mu.Unlock()
			close(s.pushDone)
		}()
	}

	for nc := range chans {
		switch nc.ChannelType() {
		case "session":
			go s.serveSession(nc)
		case "direct-tcpip":
			go s.serveForward(nc)
		default:
			_ = nc.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}
}

func (s *testServer) serveSession(nc ssh.NewChannel) {
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		_ = ssh.Unmarshal(req.Payload, &payload)
		_ = req.Reply(true, nil)

		out := "ok:" + payload.Command
		status := uint32(0)
		if strings.Contains(payload.Command, "fail") {
			out = "boom"
			status = 1
		}
		_, _ = io.WriteString(ch, out)
		// Half-close before the exit status so the payload is flushed ahead
		// of it, which is what a real sshd does.
		_ = ch.CloseWrite()
		_, _ = ch.SendRequest("exit-status", false,
			ssh.Marshal(struct{ Status uint32 }{status}))
		return
	}
}

// serveForward answers a direct-tcpip channel by echoing, which is enough to
// prove bytes traverse the tunnel in both directions.
func (s *testServer) serveForward(nc ssh.NewChannel) {
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	defer ch.Close()
	buf := make([]byte, 1024)
	for {
		n, err := ch.Read(buf)
		if n > 0 {
			_, _ = ch.Write(append([]byte("echo:"), buf[:n]...))
		}
		if err != nil {
			return
		}
	}
}

// forwardTarget reads the direct-tcpip payload, for tests that assert where
// the client asked to connect.
func forwardTarget(payload []byte) (string, uint32) {
	if len(payload) < 4 {
		return "", 0
	}
	n := binary.BigEndian.Uint32(payload[:4])
	if int(4+n+4) > len(payload) {
		return "", 0
	}
	host := string(payload[4 : 4+n])
	port := binary.BigEndian.Uint32(payload[4+n : 8+n])
	return host, port
}
