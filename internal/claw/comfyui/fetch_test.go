// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package comfyui

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/secret"
)

// A model name comes out of a workflow file, which operators download from
// strangers and open without reading. SafeName rejects traversal and null
// bytes and says nothing about shell metacharacters, so the log lines — which
// look like output rather than like commands — were the way in. The fetch runs
// as root on a host holding the operator's Hugging Face token.
func TestAWorkflowNameCannotReachTheShell(t *testing.T) {
	hostile := "sdxl$(id > /tmp/pwned).safetensors`whoami`;rm -rf /"
	d := Download{
		Dir:   ModelsDir,
		Items: []Item{{URL: "https://example.invalid/a", Name: hostile, Bytes: 10}},
	}
	script, err := d.Script(false)
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	// Every appearance of the name must be inside single quotes, which is the
	// one context where none of these characters mean anything.
	for _, bad := range []string{"$(id", "`whoami`", ";rm -rf /"} {
		for _, line := range strings.Split(script, "\n") {
			if !strings.Contains(line, bad) {
				continue
			}
			if !quotedIn(line, bad) {
				t.Errorf("a workflow name reaches the shell unquoted:\n  %s", line)
			}
		}
	}
	// And the script must still be usable: the markers the log parser reads
	// are unchanged.
	for _, want := range []string{"printf 'fetch %s", "printf 'FAILED %s", "SHORT"} {
		if !strings.Contains(script, want) {
			t.Errorf("the fetch log lost %q, which the progress parser reads", want)
		}
	}
}

// quotedIn reports whether every occurrence of sub in line falls inside a
// single-quoted shell word.
func quotedIn(line, sub string) bool {
	for i := 0; i+len(sub) <= len(line); i++ {
		if line[i:i+len(sub)] != sub {
			continue
		}
		if strings.Count(line[:i], "'")%2 == 0 {
			return false // an even number of quotes before it: not inside one
		}
	}
	return true
}

// set -e is the backstop for the steps that carry no explicit guard. Without
// it a failed rename left the model absent and the script ran on to touch the
// completion marker, so ComfyUI was launched against a bundle that was never
// there.
func TestAFetchThatCannotFinishNeverMarksItselfDone(t *testing.T) {
	d := Download{
		Dir:   ModelsDir,
		Items: []Item{{URL: "https://example.invalid/a", Name: "a.safetensors", Bytes: 10}},
	}
	script, err := d.Script(false)
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	if !strings.HasPrefix(script, "set -eu\n") {
		t.Error("the fetch script does not fail on an unguarded command error")
	}
	mv := strings.Index(script, "mv ")
	done := strings.Index(script, "touch ")
	if mv < 0 || done < 0 || mv > done {
		t.Fatal("the script no longer renames before marking itself done")
	}
	if !strings.Contains(script[mv:done], "exit 1") {
		t.Error("a failed rename does not stop the script reaching the marker")
	}
}

// A cap that silently drops what it cannot list is worse than no cap, because
// the files it dropped are destroyed moments later and the sync reports a
// clean sweep. The cap stays; what changed is that hitting it is a fact the
// teardown can see.
func TestAnOutputListingThatWasCutShortSaysSo(t *testing.T) {
	var rows strings.Builder
	for i := 0; i < maxListed+3; i++ {
		fmt.Fprintf(&rows, "10\t1700000000\tr%04d.png\n", i)
	}
	out := rows.String()
	f := &fakeSession{rule: func(string) (string, error) { return out, nil }}
	arts, truncated, err := List(context.Background(), f, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !truncated {
		t.Error("a listing that hit the cap reported itself complete")
	}
	if len(arts) > maxListed {
		t.Errorf("listed %d artefacts past a cap of %d", len(arts), maxListed)
	}

	// And a listing that fits must not claim it was cut short, or every
	// teardown would refuse.
	short := &fakeSession{rule: func(string) (string, error) {
		return "10\t1700000000\tone.png\n", nil
	}}
	if _, cut, err := List(context.Background(), short, ""); err != nil || cut {
		t.Errorf("a listing of one file reported truncated=%v err=%v", cut, err)
	}
}

// A non-200 from the queue is a failed poll, not an empty one. They shared a
// return value, so a ComfyUI busy enough to shed a request answered "nothing
// outstanding" — the reading least likely to be true at that moment.
func TestANonOKQueueIsNotAnEmptyQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := &Client{Addr: strings.TrimPrefix(srv.URL, "http://")}
	q, err := c.Queue(context.Background())
	if err == nil {
		t.Fatal("a 503 was reported as a successful, empty queue")
	}
	if q.Busy() {
		t.Error("a failed poll reported work outstanding")
	}
}

// Size was being used as identity. ComfyUI restarts its output counter, so a
// later session renders a different image under the same name — and if the
// lengths match, the newer one was reported as already held and then
// destroyed with the host.
func TestARenderIsNotSkippedJustForMatchingInSize(t *testing.T) {
	local := t.TempDir()
	dest := filepath.Join(local, "larri_00001_.png")
	old := time.Unix(1700000000, 0)
	if err := os.WriteFile(dest, []byte("AAAA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dest, old, old); err != nil {
		t.Fatal(err)
	}

	// The host holds a file of the same name and the same size, rendered
	// after the local copy was taken.
	newer := old.Add(time.Hour)
	f := &fakeSession{rule: func(cmd string) (string, error) {
		if strings.Contains(cmd, "find ") {
			return fmt.Sprintf("4\t%d\tlarri_00001_.png\n", newer.Unix()), nil
		}
		return base64.StdEncoding.EncodeToString([]byte("BBBB")), nil
	}}
	res, err := Sync(context.Background(), f, local, SyncOptions{})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("a newer render of the same size was skipped: %+v", res.Skipped)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "BBBB" {
		t.Errorf("local file is %q; the host's newer render was not collected", got)
	}
}

// Clearing the credential is the withdrawal of a token from a machine the
// operator does not own. A discarded error meant the file could survive on an
// adopted host while LARRI reported nothing wrong (invariant 9).
func TestAFailedCredentialRemovalIsReported(t *testing.T) {
	f := &fakeSession{rule: func(cmd string) (string, error) {
		if strings.Contains(cmd, "rm -f") {
			return "", errors.New("read-only file system")
		}
		return "", nil
	}}
	if err := WriteCredential(context.Background(), f, secret.Secret{}); err == nil {
		t.Error("a credential that could not be removed was reported as cleared")
	}
	// And the ordinary case still succeeds.
	ok := &fakeSession{rule: func(string) (string, error) { return "", nil }}
	if err := WriteCredential(context.Background(), ok, secret.Secret{}); err != nil {
		t.Errorf("clearing an absent credential failed: %v", err)
	}
}
