// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

// Sealer encrypts and decrypts a label payload.
//
// It is an interface so that core does not own key management: the key comes
// from the same place every other secret does (FR-SEC-01), and this package
// only needs to know that something can seal bytes.
type Sealer interface {
	Seal(plaintext []byte) (string, error)
	Open(token string) ([]byte, error)
}

// AEADSealer seals label payloads with AES-256-GCM.
type AEADSealer struct{ aead cipher.AEAD }

// NewAEADSealer builds a sealer from a 32-byte key.
func NewAEADSealer(key []byte) (*AEADSealer, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("core: label key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("core: build cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("core: build gcm: %w", err)
	}
	return &AEADSealer{aead: aead}, nil
}

// NewLabelKey generates a label encryption key.
func NewLabelKey() ([]byte, error) {
	k := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, k); err != nil {
		return nil, fmt.Errorf("core: read entropy: %w", err)
	}
	return k, nil
}

// Seal encrypts and encodes a payload. The nonce is random per call and
// prepended, so two rigs with identical details do not produce identical
// labels — which would otherwise let a host correlate them.
func (s *AEADSealer) Seal(plaintext []byte) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("core: read nonce: %w", err)
	}
	ct := s.aead.Seal(nonce, nonce, plaintext, nil)
	return base64.RawURLEncoding.EncodeToString(ct), nil
}

// Open decodes and decrypts. A payload that does not authenticate is an error
// rather than a best-effort parse: a label that has been tampered with should
// not be quietly believed.
func (s *AEADSealer) Open(token string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("core: decode sealed label: %w", err)
	}
	n := s.aead.NonceSize()
	if len(raw) < n {
		return nil, fmt.Errorf("core: sealed label too short")
	}
	pt, err := s.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return nil, fmt.Errorf("core: sealed label failed authentication: %w", err)
	}
	return pt, nil
}
