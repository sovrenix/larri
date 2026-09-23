// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.sovrenix.com/larri/internal/secret"
)

// ClientKeyEnv supplies the client credential from the environment.
const ClientKeyEnv = "LARRI_CLIENT_KEY"

// ClientKeySource says where the credential came from, for disclosure.
type ClientKeySource string

const (
	ClientKeyFromEnv   ClientKeySource = "environment"
	ClientKeyFromFile  ClientKeySource = "file"
	ClientKeyStored    ClientKeySource = "stored"
	ClientKeyCreated   ClientKeySource = "created"
	ClientKeyEphemeral ClientKeySource = "ephemeral"
	clientKeyFileName                  = "client.key"
	clientKeyBytes                     = 32
)

// ClientKeyPath is where a generated credential is kept, beside the
// configuration rather than inside it.
//
// Its own file for two reasons. A config file is a thing operators open,
// paste into issues and check into dotfile repositories, and a credential in
// it travels with all of that. And the file needs 0600, which is the wrong
// mode for a file people are expected to edit.
func ClientKeyPath() string {
	if p := os.Getenv("LARRI_CLIENT_KEY_PATH"); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(Path()), clientKeyFileName)
}

// ResolveClientKey returns the credential local clients authenticate with,
// creating and storing one the first time.
//
// **Stable across rigs, and that is the entire point.** The credential guards
// the local listener, and a client is configured against it *once* — pasted
// into a transcription app, written into an IDE's settings, kept in a
// bookmarked URL. Minting a fresh one per rig, which is what happened before
// this existed, means every teardown silently invalidates every client's
// configuration: the endpoint is still at the same address, still answering,
// and rejects them. That is precisely the churn the fixed local port exists to
// prevent (invariant 3, P3), arriving through the credential instead of
// through the address.
//
// The rig token is the opposite and stays that way: ephemeral, one per rig,
// rotated on every replacement, and never seen by a client. The proxy is the
// boundary between the two (FR-SEC-22).
//
// Resolution order matches the label key — environment, then a file the
// environment names — with one addition: absence is not an outcome here,
// because the local API key is mandatory (FR-SEC-09). What absence means is
// "make one and remember it".
func ResolveClientKey(env func(string) string) (secret.Secret, ClientKeySource, error) {
	if env == nil {
		env = os.Getenv
	}
	if v := strings.TrimSpace(env(ClientKeyEnv)); v != "" {
		return secret.New(v), ClientKeyFromEnv, nil
	}
	if path := strings.TrimSpace(env(ClientKeyEnv + "_FILE")); path != "" {
		if err := secureClientKeyFile(path); err != nil {
			return secret.Secret{}, ClientKeyEphemeral, err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return secret.Secret{}, ClientKeyEphemeral,
				fmt.Errorf("config: read client key: %w", err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return secret.Secret{}, ClientKeyEphemeral,
				fmt.Errorf("config: %s is empty", path)
		}
		return secret.New(v), ClientKeyFromFile, nil
	}

	path := ClientKeyPath()
	if _, err := os.Stat(path); err == nil {
		if err := secureClientKeyFile(path); err != nil {
			return secret.Secret{}, ClientKeyEphemeral, err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return secret.Secret{}, ClientKeyEphemeral,
				fmt.Errorf("config: read client key: %w", err)
		}
		if v := strings.TrimSpace(string(b)); v != "" {
			return secret.New(v), ClientKeyStored, nil
		}
	} else if !os.IsNotExist(err) {
		return secret.Secret{}, ClientKeyEphemeral,
			fmt.Errorf("config: read client key: %w", err)
	}
	key, err := generateClientKey()
	if err != nil {
		return secret.Secret{}, ClientKeyEphemeral, err
	}
	switch stored, err := createClientKey(path, key); {
	case err == nil && stored != "":
		// Somebody else created it between the check above and this call.
		// Theirs is the one on disk, so theirs is the one every future
		// session will read — and a credential that disagrees with the file
		// is the churn this whole function exists to prevent (invariant 8).
		return secret.New(stored), ClientKeyStored, nil
	case err != nil:
		// A credential that could not be stored still works for this session;
		// it simply will not survive it. Reported rather than fatal, because
		// refusing to bring a rig up over a file permission is worse than
		// bringing one up whose clients need reconfiguring next time.
		return secret.New(key), ClientKeyEphemeral, err
	}
	return secret.New(key), ClientKeyCreated, nil
}

// createClientKey writes the credential only if nothing else got there first,
// and returns what is on disk when something did.
//
// O_EXCL rather than a temporary file and a rename, because the rename was the
// race. Two first runs — a claw in one terminal, another in the next — both
// saw no file, both generated a key, and both renamed over the top. Each
// process then returned the value it made while the file held one of them, so
// the clients configured by the loser were invalidated by the next rig that
// read the file.
//
// The kernel decides who wins, and the loser reads the winner's value rather
// than its own.
func createClientKey(path, key string) (stored string, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("config: create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return "", fmt.Errorf("config: read client key: %w", rerr)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			// Created but not yet written — the winner is between the two
			// calls. Nothing to read, so this session uses its own value and
			// says so rather than returning an empty credential.
			return "", fmt.Errorf("config: %s is empty", path)
		}
		return v, nil
	}
	if err != nil {
		return "", fmt.Errorf("config: write %s: %w", path, err)
	}
	if _, err := f.WriteString(key + "\n"); err != nil {
		f.Close()
		return "", fmt.Errorf("config: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("config: write %s: %w", path, err)
	}
	return "", nil
}

func generateClientKey() (string, error) {
	b := make([]byte, clientKeyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("config: generate client key: %w", err)
	}
	// Prefixed so it is recognisable in a config file somebody is debugging,
	// and URL-safe so it survives being pasted into a query string.
	return "larri-" + base64.RawURLEncoding.EncodeToString(b), nil
}

func secureClientKeyFile(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("config: secure %s: %w", path, err)
	}
	return nil
}

// Describe says where the credential came from, for the bring-up disclosure.
func (s ClientKeySource) Describe() string {
	switch s {
	case ClientKeyFromEnv:
		return "from " + ClientKeyEnv
	case ClientKeyFromFile:
		return "from " + ClientKeyEnv + "_FILE"
	case ClientKeyStored:
		return "stored in " + ClientKeyPath()
	case ClientKeyCreated:
		return "created, stored in " + ClientKeyPath()
	default:
		return "this session only: clients will need reconfiguring next time"
	}
}
