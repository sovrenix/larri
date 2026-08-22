// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package sshx speaks SSH in-process.
//
// LARRI does not drive an `ssh` binary, and the reason is that most of the
// hardening then stops being configuration and becomes structure. An option
// you must remember to pass can be forgotten, overridden by the operator's
// ~/.ssh/config, or silently unsupported by whichever ssh build is installed.
// A capability that was never implemented can be none of those things.
//
// Concretely, and these are the rows that matter (§15.5.2):
//
//   - Agent forwarding is not implemented. No agent is ever contacted. This is
//     the most dangerous default in this threat model: a forwarded agent lets
//     a host with root authenticate AS THE OPERATOR to every system that
//     trusts their key.
//   - X11 and remote forwarding are not implemented.
//   - ~/.ssh/config is never read, so a global `ForwardAgent yes` cannot
//     re-enable what this package declines to build.
//   - Server-initiated channels and global requests are rejected, so a
//     malicious host cannot open a channel back toward the operator.
package sshx

import (
	"context"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"

	"go.sovrenix.com/larri/internal/errs"
)

// Config describes one connection to a rented host.
type Config struct {
	Host string
	Port int
	User string
	Key  *KeyPair

	// HostKey is the pinned public key. Required.
	//
	// There is no "accept anything" mode and no TOFU fallback in this type:
	// the caller pins on first connect and records the fingerprint, so a key
	// that changes later is a compromise rather than a prompt (FR-SEC-04).
	HostKey ssh.PublicKey

	// Timeout bounds the handshake.
	Timeout time.Duration
}

// Client is a live connection, carrying many channels.
//
// One connection serves the tunnel, bootstrap exec, log streaming, and metrics
// collection (§17.4). Multiplexing is native here, where the binary would have
// needed ControlMaster and a socket on disk.
type Client struct {
	ssh  *ssh.Client
	conf Config
}

// hostKeyAlgorithms is the host key preference, and it is shared by the scan
// and the dial for a reason that cost a live run to find.
//
// A scan that leaves this unset negotiates whatever Go prefers by default —
// which puts ECDSA ahead of Ed25519 — while a dial that pins Ed25519 asks the
// server for a different key. Both keys are legitimate and belong to the same
// host, so the comparison fails on every attempt, deterministically, and
// reports itself as `host key mismatch`: a phrase that describes an attack and
// here described a configuration error.
//
// Whatever a pin is compared against must be obtained the same way it will be
// presented. One list, used twice.
var hostKeyAlgorithms = []string{
	ssh.KeyAlgoED25519,
	ssh.CertAlgoED25519v01,
	ssh.KeyAlgoRSASHA256,
}

// hardenedConfig builds the client configuration.
//
// Algorithms are named explicitly rather than left to defaults, because the
// far end is an image LARRI publishes and there is no legacy peer to
// accommodate.
func hardenedConfig(c Config) (*ssh.ClientConfig, error) {
	if c.Key == nil {
		return nil, fmt.Errorf("sshx: no key pair")
	}
	if c.HostKey == nil {
		return nil, fmt.Errorf("sshx: no pinned host key")
	}
	user := c.User
	if user == "" {
		user = "root"
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &ssh.ClientConfig{
		User: user,
		// Public key only. No password or keyboard-interactive method is
		// offered, so a host that asks for one gets nothing.
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(c.Key.Signer())},
		HostKeyCallback:   ssh.FixedHostKey(c.HostKey),
		HostKeyAlgorithms: hostKeyAlgorithms,
		Config: ssh.Config{
			KeyExchanges: []string{
				"curve25519-sha256", "curve25519-sha256@libssh.org",
			},
			Ciphers: []string{
				"chacha20-poly1305@openssh.com",
				"aes256-gcm@openssh.com",
				"aes128-gcm@openssh.com",
			},
			MACs: []string{"hmac-sha2-256-etm@openssh.com"},
		},
		Timeout: timeout,
	}, nil
}

// Dial opens a connection to the host.
func Dial(ctx context.Context, c Config) (*Client, error) {
	conf, err := hardenedConfig(c)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(c.Host, fmt.Sprint(c.Port))

	d := net.Dialer{Timeout: conf.Timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, errs.New(errs.ClassHostFailure, "sshx.Dial", err)
	}
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, conf)
	if err != nil {
		conn.Close()
		return nil, errs.New(errs.ClassHostFailure, "sshx.Dial", err)
	}

	// Reject everything the server tries to initiate.
	//
	// ssh.NewClient would discard these, but doing it explicitly is the point:
	// a compromised host may request a channel back toward the operator, and
	// the only correct answer is no. Nothing on this connection flows in that
	// direction (§15.8.1).
	go rejectChannels(chans)
	go ssh.DiscardRequests(reqs)

	return &Client{ssh: ssh.NewClient(sc, nil, nil), conf: c}, nil
}

func rejectChannels(chans <-chan ssh.NewChannel) {
	for ch := range chans {
		_ = ch.Reject(ssh.Prohibited, "larri: server-initiated channels are not accepted")
	}
}

// Close releases the connection.
func (c *Client) Close() error {
	if c.ssh == nil {
		return nil
	}
	return c.ssh.Close()
}

// Keepalive sends periodic requests so a dead connection is noticed rather
// than waited on. Feeds the supervisor's 10 s tunnel liveness check (§12).
func (c *Client) Keepalive(ctx context.Context, every time.Duration) error {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if _, _, err := c.ssh.SendRequest("keepalive@larri", true, nil); err != nil {
				return errs.New(errs.ClassHostFailure, "sshx.Keepalive", err)
			}
		}
	}
}

// HostKeyFingerprint returns the pinned key's fingerprint, for the journal.
func (c *Client) HostKeyFingerprint() string {
	if c.conf.HostKey == nil {
		return ""
	}
	return ssh.FingerprintSHA256(c.conf.HostKey)
}

// ScanHostKey retrieves a host's public key for pinning on first connect.
//
// This is the TOFU window, and it is narrow rather than absent: LARRI learns
// the address from the provider API over verified TLS moments before
// connecting. It is not zero, and §15.8.2 states why pinning cannot exclude
// the provider itself.
func ScanHostKey(ctx context.Context, host string, port int, timeout time.Duration) (ssh.PublicKey, error) {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, errs.New(errs.ClassHostFailure, "sshx.ScanHostKey", err)
	}
	defer conn.Close()

	var found ssh.PublicKey
	conf := &ssh.ClientConfig{
		User: "larri-scan",
		Auth: []ssh.AuthMethod{},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			found = key
			return nil
		},
		// The same preference the dial will use. Scanning with different
		// algorithms pins a key the connection will never be offered.
		HostKeyAlgorithms: hostKeyAlgorithms,
		Timeout:           timeout,
	}
	// The handshake is expected to fail at authentication; the host key is
	// presented before that, which is all this needs.
	sc, _, _, err := ssh.NewClientConn(conn, addr, conf)
	if sc != nil {
		sc.Close()
	}
	if found == nil {
		return nil, errs.Newf(errs.ClassHostFailure, "sshx.ScanHostKey",
			"host presented no key: %v", err)
	}
	return found, nil
}
