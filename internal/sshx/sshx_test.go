// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func dialTest(t *testing.T, s *testServer, hostKey ssh.PublicKey) (*Client, error) {
	t.Helper()
	kp, err := NewKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if hostKey == nil {
		hostKey = s.HostKey
	}
	return Dial(context.Background(), Config{
		Host: "127.0.0.1", Port: s.Port(), User: "root",
		Key: kp, HostKey: hostKey, Timeout: 10 * time.Second,
	})
}

func TestDialAndExec(t *testing.T) {
	s := newTestServer(t, nil)
	c, err := dialTest(t, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	out, err := c.Session().Run(context.Background(), "nvidia-smi")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "ok:nvidia-smi" {
		t.Errorf("output = %q", out)
	}
}

// FR-SEC-04: a host key that does not match the pin is a compromise, not a
// prompt. There is no accept-anything mode in this package to fall back to.
func TestWrongHostKeyIsRefused(t *testing.T) {
	s := newTestServer(t, nil)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	otherSigner, _ := ssh.NewSignerFromKey(other)

	_, err := dialTest(t, s, otherSigner.PublicKey())
	if err == nil {
		t.Fatal("a mismatched host key must abort the connection")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "host key") {
		t.Logf("error was: %v", err)
	}
}

func TestPinnedHostKeyIsRequired(t *testing.T) {
	kp, _ := NewKeyPair()
	_, err := Dial(context.Background(), Config{
		Host: "127.0.0.1", Port: 22, Key: kp, // no HostKey
	})
	if err == nil {
		t.Fatal("dialling without a pinned host key must be refused")
	}
}

// The most dangerous default in this threat model. A forwarded agent lets a
// host with root authenticate as the operator everywhere their key is
// trusted, so the capability is not implemented and a host that asks gets a
// rejection rather than a channel.
func TestServerInitiatedChannelsAreRejected(t *testing.T) {
	s := newTestServer(t, func(s *testServer) { s.PushChannel = true })
	c, err := dialTest(t, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	select {
	case <-s.pushDone:
	case <-time.After(5 * time.Second):
		t.Fatal("server never attempted to open a channel")
	}
	s.mu.Lock()
	pushErr := s.pushErr
	s.mu.Unlock()

	if pushErr == nil {
		t.Fatal("the host opened a channel back toward the operator; it must be refused")
	}
	t.Logf("host's channel request refused: %v", pushErr)
}

// Password authentication is not offered, so a host advertising it gets no
// credential to collect.
func TestPasswordAuthIsNeverOffered(t *testing.T) {
	s := newTestServer(t, func(s *testServer) { s.AllowPassword = true })
	kp, _ := NewKeyPair()
	conf, err := hardenedConfig(Config{Key: kp, HostKey: s.HostKey})
	if err != nil {
		t.Fatal(err)
	}
	if len(conf.Auth) != 1 {
		t.Fatalf("exactly one auth method should be offered, got %d", len(conf.Auth))
	}
	// Connecting still works, via the key, not the password.
	c, err := dialTest(t, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

// The structural replacement for ExitOnForwardFailure: the bind happens before
// anything is declared healthy, so a port already in use is an error here
// rather than a tunnel that looks alive and carries nothing.
func TestListenFailsWhenPortIsTaken(t *testing.T) {
	s := newTestServer(t, nil)
	c, err := dialTest(t, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Occupy a port, as a previous run would.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	taken := blocker.Addr().(*net.TCPAddr).Port

	if _, err := c.Listen(taken, 8000); err == nil {
		t.Fatal("binding an occupied port must fail rather than appear to succeed")
	}
}

// Traffic actually traverses the tunnel, and the local end binds loopback
// only.
func TestForwardCarriesTrafficOnLoopback(t *testing.T) {
	s := newTestServer(t, nil)
	c, err := dialTest(t, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	fwd, err := c.Listen(0, 8000) // 0: let the kernel choose
	if err != nil {
		t.Fatal(err)
	}
	defer fwd.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fwd.Serve(ctx)

	addr := fwd.LocalAddr().(*net.TCPAddr)
	if !addr.IP.IsLoopback() {
		t.Fatalf("forward bound %s; it must bind loopback only", addr.IP)
	}

	conn, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "echo:ping" {
		t.Errorf("round trip through the tunnel = %q, want echo:ping", got)
	}
}

func TestRunReturnsOutputWithTheError(t *testing.T) {
	s := newTestServer(t, nil)
	c, err := dialTest(t, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	out, err := c.Session().Run(context.Background(), "please-fail")
	if err == nil {
		t.Fatal("a non-zero exit must be an error")
	}
	// A failed command's output is usually the only diagnosis available from
	// a host the operator cannot casually log into.
	if string(out) != "boom" {
		t.Errorf("output should accompany the error, got %q", out)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("the error should carry the first line: %v", err)
	}
}

func TestEphemeralKeyPair(t *testing.T) {
	a, err := NewKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewKeyPair()
	if a.AuthorizedKey() == b.AuthorizedKey() {
		t.Fatal("each rig must get a distinct identity, or revocation is not automatic")
	}
	if !strings.HasPrefix(a.AuthorizedKey(), "ssh-ed25519 ") {
		t.Errorf("authorized_keys line = %q", a.AuthorizedKey())
	}
	if !strings.HasPrefix(a.Fingerprint(), "SHA256:") {
		t.Errorf("fingerprint = %q", a.Fingerprint())
	}
	script := a.OnStartScript()
	if !strings.Contains(script, a.AuthorizedKey()) {
		t.Error("the onstart script must install this rig's key")
	}
	// Installed per instance rather than on the account, so it dies with the
	// rig instead of accumulating across every machine ever rented.
	if !strings.Contains(script, "authorized_keys") {
		t.Error("the key should land in authorized_keys")
	}
}

func TestScanHostKeyRetrievesTheKeyForPinning(t *testing.T) {
	s := newTestServer(t, nil)
	key, err := ScanHostKey(context.Background(), "127.0.0.1", s.Port(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(key.Marshal()) != string(s.HostKey.Marshal()) {
		t.Fatal("scanned key must match the server's")
	}
	// And the scanned key works as a pin.
	c, err := dialTest(t, s, key)
	if err != nil {
		t.Fatalf("the scanned key should pin successfully: %v", err)
	}
	c.Close()
}

func TestKeepaliveDetectsADeadConnection(t *testing.T) {
	s := newTestServer(t, nil)
	c, err := dialTest(t, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Close() // the host goes away

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Keepalive(ctx, 50*time.Millisecond); err == nil {
		t.Fatal("keepalive must report a dead connection, which is what the supervisor watches")
	}
}
