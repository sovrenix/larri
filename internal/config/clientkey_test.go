// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// isolate points the credential at a directory of this test's own.
func isolate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client.key")
	t.Setenv("LARRI_CLIENT_KEY_PATH", path)
	return path
}

// The whole reason this exists. A client is configured against this value
// once; a fresh one per rig invalidates every client's configuration on every
// teardown, which is the churn the fixed local port exists to prevent arriving
// through the credential instead of the address.
func TestTheCredentialSurvivesTheRigThatCreatedIt(t *testing.T) {
	path := isolate(t)

	first, src, err := ResolveClientKey(noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if src != ClientKeyCreated {
		t.Errorf("source = %q, want created on a first run", src)
	}
	if first.Empty() {
		t.Fatal("no credential was produced, and the local api key is mandatory")
	}

	second, src2, err := ResolveClientKey(noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if second.Reveal() != first.Reveal() {
		t.Error("the second rig got a different credential, so every client would need reconfiguring")
	}
	if src2 != ClientKeyStored {
		t.Errorf("source = %q, want stored on a later run", src2)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("nothing was stored: %v", err)
	}
}

// A credential readable by other accounts on a shared machine is a request
// that fires, and for LARRI a request that fires is a request that spends.
func TestTheStoredCredentialIsReadableOnlyByItsOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	path := isolate(t)
	if _, _, err := ResolveClientKey(noEnv); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}
}

// The environment wins, so an operator can pin one credential across machines
// without copying a file around.
func TestTheEnvironmentOverridesWhatIsStored(t *testing.T) {
	isolate(t)
	if _, _, err := ResolveClientKey(noEnv); err != nil {
		t.Fatal(err)
	}
	env := func(k string) string {
		if k == ClientKeyEnv {
			return "pinned-by-hand"
		}
		return ""
	}
	got, src, err := ResolveClientKey(env)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reveal() != "pinned-by-hand" {
		t.Errorf("credential = %q, the environment was ignored", got.Reveal())
	}
	if src != ClientKeyFromEnv {
		t.Errorf("source = %q", src)
	}
}

func TestACredentialFileNamedByTheEnvironmentIsRead(t *testing.T) {
	isolate(t)
	f := filepath.Join(t.TempDir(), "elsewhere.key")
	if err := os.WriteFile(f, []byte("  from-a-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(k string) string {
		if k == ClientKeyEnv+"_FILE" {
			return f
		}
		return ""
	}
	got, src, err := ResolveClientKey(env)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reveal() != "from-a-file" {
		t.Errorf("credential = %q, whitespace was not trimmed", got.Reveal())
	}
	if src != ClientKeyFromFile {
		t.Errorf("source = %q", src)
	}
}

func TestAPermissiveStoredCredentialIsRepairedBeforeUse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	path := isolate(t)
	if err := os.WriteFile(path, []byte("stored-key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, src, err := ResolveClientKey(noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reveal() != "stored-key" || src != ClientKeyStored {
		t.Fatalf("got %q from %q", got.Reveal(), src)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}
}

// A named file that cannot be read is a mistake worth reporting rather than
// silently replacing with a generated value the operator did not ask for.
func TestAnUnreadableNamedFileIsReported(t *testing.T) {
	isolate(t)
	env := func(k string) string {
		if k == ClientKeyEnv+"_FILE" {
			return filepath.Join(t.TempDir(), "absent.key")
		}
		return ""
	}
	if _, _, err := ResolveClientKey(env); err == nil {
		t.Fatal("a missing credential file resolved successfully")
	}
}

// Storing it may fail — a read-only home, a full disk — and that must not
// refuse the rig. The session works; it simply will not be the same
// credential next time, and the source says so.
func TestAnUnstorableCredentialStillWorksForThisSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	t.Setenv("LARRI_CLIENT_KEY_PATH", filepath.Join(locked, "sub", "client.key"))

	key, src, err := ResolveClientKey(noEnv)
	if err == nil {
		t.Skip("this environment allowed the write anyway")
	}
	if key.Empty() {
		t.Error("a credential that could not be stored was not produced at all")
	}
	if src != ClientKeyEphemeral {
		t.Errorf("source = %q, want ephemeral", src)
	}
	if !strings.Contains(src.Describe(), "reconfiguring") {
		t.Errorf("the disclosure does not say what the operator will have to redo: %q",
			src.Describe())
	}
}

// Recognisable when somebody is staring at a config file wondering what a
// string is, and safe in a query string, since a browser session URL carries
// one.
func TestAGeneratedCredentialIsRecognisableAndURLSafe(t *testing.T) {
	isolate(t)
	key, _, err := ResolveClientKey(noEnv)
	if err != nil {
		t.Fatal(err)
	}
	v := key.Reveal()
	if !strings.HasPrefix(v, "larri-") {
		t.Errorf("credential = %q, which names nothing", v)
	}
	if strings.ContainsAny(v, "+/=") {
		t.Errorf("credential = %q contains characters a url would escape", v)
	}
	if len(v) < 40 {
		t.Errorf("credential = %q is short enough to be worth guessing", v)
	}
}
