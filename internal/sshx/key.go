// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// KeyPair is an ephemeral SSH identity, generated per rig.
//
// The operator's long-lived key never reaches rented hardware (FR-SEC-17).
// Public keys are not secret, so uploading one discloses nothing — but reusing
// one identity across rented hosts means every host learns the same public
// identity, and access outlives the rig unless someone remembers to clean
// authorized_keys on a machine they no longer rent. A fresh pair makes
// revocation automatic: destroy the rig, discard the key, and the credential
// that could reach it stops existing.
type KeyPair struct {
	priv   ed25519.PrivateKey
	signer ssh.Signer
}

// NewKeyPair generates an ed25519 identity.
func NewKeyPair() (*KeyPair, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sshx: generate key: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, fmt.Errorf("sshx: build signer: %w", err)
	}
	return &KeyPair{priv: priv, signer: signer}, nil
}

// Signer is the authentication method for this identity.
func (k *KeyPair) Signer() ssh.Signer { return k.signer }

// AuthorizedKey renders the public half in authorized_keys format, for
// injection into the instance at boot.
func (k *KeyPair) AuthorizedKey() string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k.signer.PublicKey())))
}

// Fingerprint is the SHA256 fingerprint of the public half, for logs and the
// journal. Safe to print: it identifies the key without disclosing it.
func (k *KeyPair) Fingerprint() string {
	return ssh.FingerprintSHA256(k.signer.PublicKey())
}

// OnStartScript renders the command that installs this key on the host.
//
// The key is installed per instance rather than registered on the Vast account
// because account-level keys are shared by every instance the operator ever
// rents, which would defeat the point of generating a fresh pair.
func (k *KeyPair) OnStartScript() string {
	return "mkdir -p /root/.ssh && chmod 700 /root/.ssh && " +
		"echo '" + k.AuthorizedKey() + "' >> /root/.ssh/authorized_keys && " +
		"chmod 600 /root/.ssh/authorized_keys"
}
