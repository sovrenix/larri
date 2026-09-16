// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package clientkeys keeps the API keys local clients use against the stable
// /v1 endpoint.
//
// A client key is stable across rigs: an IDE or chat client is configured with
// one once and never again (invariant 3). Before this, every bring-up minted a
// fresh key, so every client config went stale at every `larri up` — the churn
// the fixed local endpoint exists to prevent — and `larri resume` minted one it
// never printed, leaving a reconnected rig that no client could reach.
//
// Only a SHA-256 hash of each key is stored. The key is shown once, when it is
// created, and cannot be shown again; a lost key is revoked and replaced. A
// 256-bit random key needs no slow hash: there is nothing to brute-force.
package clientkeys

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"syscall"
	"time"

	"go.sovrenix.com/larri/internal/secret"
)

// FileName is where the keys live, inside the state directory.
const FileName = "client-keys.json"

// Key is one stored client key: its name and the hash of its value.
type Key struct {
	Name    string    `json:"name"`
	SHA256  string    `json:"sha256"`
	Created time.Time `json:"created"`
}

// ErrExists is returned when creating a key under a name already in use.
var ErrExists = errors.New("client key exists")

// ErrUnknown is returned when revoking a name that holds no key.
var ErrUnknown = errors.New("no such client key")

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Store is the key file in one state directory.
type Store struct {
	dir string
	now func() time.Time

	mu      sync.Mutex
	modTime time.Time
	size    int64
	cached  []Key
}

// Open returns the key store in dir. Nothing is read until it is used.
func Open(dir string) *Store {
	return &Store{dir: dir, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Store) path() string { return filepath.Join(s.dir, FileName) }

// Hash is the stored form of a key.
func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Create makes a new key under name and returns its value, which is never
// available again.
func (s *Store) Create(name string) (secret.Secret, error) {
	if !validName.MatchString(name) {
		return secret.Secret{}, fmt.Errorf("clientkeys: name %q: expected 1-32 of a-z 0-9 _ -, starting with a letter or digit", name)
	}
	unlock, err := s.lock()
	if err != nil {
		return secret.Secret{}, err
	}
	defer unlock()
	keys, err := s.read()
	if err != nil {
		return secret.Secret{}, err
	}
	for _, k := range keys {
		if k.Name == name {
			return secret.Secret{}, fmt.Errorf("clientkeys: %s: %w: revoke it first", name, ErrExists)
		}
	}
	value, err := secret.Generate(32)
	if err != nil {
		return secret.Secret{}, err
	}
	keys = append(keys, Key{Name: name, SHA256: Hash(value.Reveal()), Created: s.now()})
	if err := s.write(keys); err != nil {
		return secret.Secret{}, err
	}
	return value, nil
}

// Check reports whether the stored keys can be read. A store that cannot be
// read matches no key, so a rig relying on it would admit no client.
func (s *Store) Check() error {
	_, err := s.read()
	return err
}

// List returns the stored keys, oldest first. Hashes are included; they are
// not secret, but nothing displays them.
func (s *Store) List() ([]Key, error) {
	keys, err := s.read()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(keys, func(i, j int) bool { return keys[i].Created.Before(keys[j].Created) })
	return keys, nil
}

// Revoke removes the key under name. A running rig stops accepting it within
// the time Match takes to notice the file changed.
func (s *Store) Revoke(name string) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	keys, err := s.read()
	if err != nil {
		return err
	}
	kept := keys[:0]
	found := false
	for _, k := range keys {
		if k.Name == name {
			found = true
			continue
		}
		kept = append(kept, k)
	}
	if !found {
		return fmt.Errorf("clientkeys: %s: %w", name, ErrUnknown)
	}
	return s.write(kept)
}

// Match reports which stored key a presented value is, if any.
//
// The file is re-read whenever it has changed, so a key created or revoked by
// another process — `larri token revoke` while `larri up` holds a rig — takes
// effect on the next request rather than at the next bring-up. Every stored
// hash is compared, in constant time, so the position of a match is not
// visible in the timing.
func (s *Store) Match(presented string) (string, bool) {
	keys := s.current()
	if len(keys) == 0 || presented == "" {
		return "", false
	}
	h := []byte(Hash(presented))
	name, ok := "", false
	for _, k := range keys {
		if subtle.ConstantTimeCompare(h, []byte(k.SHA256)) == 1 {
			name, ok = k.Name, true
		}
	}
	return name, ok
}

// current returns the keys, re-reading the file only when it has changed. A
// file that cannot be read matches nothing: a key check that cannot run
// admits no one.
func (s *Store) current() []Key {
	fi, err := os.Stat(s.path())
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.cached, s.modTime, s.size = nil, time.Time{}, 0
		return nil
	}
	if fi.ModTime().Equal(s.modTime) && fi.Size() == s.size && s.cached != nil {
		return s.cached
	}
	keys, rerr := s.read()
	if rerr != nil {
		s.cached = nil
		return nil
	}
	s.cached, s.modTime, s.size = keys, fi.ModTime(), fi.Size()
	return keys
}

func (s *Store) read() ([]Key, error) {
	b, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("clientkeys: read: %w", err)
	}
	var keys []Key
	if err := json.Unmarshal(b, &keys); err != nil {
		return nil, fmt.Errorf("clientkeys: parse %s: %w", s.path(), err)
	}
	return keys, nil
}

// write replaces the file atomically at 0600 and checks the mode it landed
// with (FR-SEC-26): the hashes are not secrets, but a file anyone can write is
// a file anyone can add a key to.
func (s *Store) write(keys []Key) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("clientkeys: %w", err)
	}
	b, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return fmt.Errorf("clientkeys: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".client-keys.*.tmp")
	if err != nil {
		return fmt.Errorf("clientkeys: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("clientkeys: %w", err)
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("clientkeys: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("clientkeys: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("clientkeys: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path()); err != nil {
		return fmt.Errorf("clientkeys: %w", err)
	}
	fi, err := os.Stat(s.path())
	if err != nil {
		return fmt.Errorf("clientkeys: %w", err)
	}
	if fi.Mode().Perm() != 0o600 {
		return fmt.Errorf("clientkeys: %s has mode %o: expected 600", s.path(), fi.Mode().Perm())
	}
	return nil
}

// lock serialises writers across processes.
func (s *Store) lock() (func(), error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, fmt.Errorf("clientkeys: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(s.dir, ".client-keys.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("clientkeys: lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("clientkeys: lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
