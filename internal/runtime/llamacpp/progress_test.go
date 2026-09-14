// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package llamacpp

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/runtime"
)

// bashSession runs the measuring command in a local bash, rooted at a
// temporary directory standing in for the host's.
type bashSession struct{ root string }

func (s bashSession) Run(ctx context.Context, cmd string) ([]byte, error) {
	cmd = strings.ReplaceAll(cmd, shellQuote(ModelDir), shellQuote(filepath.Join(s.root, ModelDir)))
	return exec.CommandContext(ctx, "bash", "-c", cmd).Output()
}
func (bashSession) Dial(context.Context, int) (io.ReadWriteCloser, error) { return nil, nil }
func (bashSession) Close() error                                          { return nil }

// A file with space reserved beyond what has been written — what XFS does to
// a growing download — reports the bytes written, not the reservation.
func TestProgressCountsBytesWrittenNotSpaceReserved(t *testing.T) {
	if _, err := exec.LookPath("fallocate"); err != nil {
		t.Skip("no fallocate")
	}
	root := t.TempDir()
	dir := filepath.Join(root, ModelDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(dir, "m-00001-of-00002.gguf")
	if err := os.WriteFile(part, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	// Reserve 64 MB past the end without changing the file's size.
	if out, err := exec.Command("fallocate", "--keep-size", "-o", "1048576", "-l", "67108864", part).CombinedOutput(); err != nil {
		t.Skipf("filesystem cannot reserve space: %s", out)
	}
	got, err := New().WeightsOnDisk(context.Background(), bashSession{root: root})
	if err != nil {
		t.Fatal(err)
	}
	if got > 2<<20 {
		t.Errorf("measured %d bytes; 1 MB was written and the rest is only reserved", got)
	}
}

// The size is what the host prints, and the host is not trusted. An answer
// past the whole download — past what an int64 holds, even — is reported as
// the whole download and no more.
func TestProgressIsBoundedByTheDownload(t *testing.T) {
	downloadPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { downloadPollInterval = 10 * time.Second })

	r := New()
	r.SetWeights(Weights{File: "m.gguf", Bytes: 10 << 30})
	sess := &scriptedSession{
		recSession: recSession{out: "llama-server"},
		slow:       60 * time.Millisecond,
		answers: map[string]string{
			"df -Pk": "999999999\n0\n",
			"du -sb": "18446744073709551615\n", // 2^64-1
		},
	}
	seen := make(chan runtime.Progress, 64)
	if err := r.Bootstrap(context.Background(), sess, spec(), core.SizingPlan{}, seen); err != nil {
		t.Fatal(err)
	}
	close(seen)
	var reported bool
	for p := range seen {
		if p.BytesTotal == 0 {
			continue
		}
		reported = true
		if p.BytesDone != p.BytesTotal || p.Percent != 100 {
			t.Errorf("progress %d of %d bytes (%.0f%%); bounded by the download", p.BytesDone, p.BytesTotal, p.Percent)
		}
	}
	if !reported {
		t.Error("no progress reported")
	}
}

// wedgedSession never answers a progress measurement until its context ends,
// the way an SSH exec to a host that has stopped responding behaves.
type wedgedSession struct{ scriptedSession }

func (s *wedgedSession) Run(ctx context.Context, cmd string) ([]byte, error) {
	if strings.Contains(cmd, "du -sb") {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.scriptedSession.Run(ctx, cmd)
}

// Progress is best-effort and must not be what a bring-up waits on. Stopping
// the sampler waited for it to exit while its du hung on the bootstrap's own
// context, which holds until the attempt deadline — up to 75 billed minutes.
func TestAWedgedProgressProbeDoesNotHoldBootstrap(t *testing.T) {
	downloadPollInterval, downloadProbeTimeout = 5*time.Millisecond, time.Hour
	t.Cleanup(func() { downloadPollInterval, downloadProbeTimeout = 10*time.Second, 15*time.Second })

	r := New()
	r.SetWeights(Weights{File: "m.gguf", Bytes: 10 << 30})
	sess := &wedgedSession{scriptedSession{
		recSession: recSession{out: "llama-server"},
		slow:       50 * time.Millisecond,
		answers:    map[string]string{"df -Pk": "999999999\n0\n"},
	}}
	done := make(chan error, 1)
	go func() { done <- r.Bootstrap(context.Background(), sess, spec(), core.SizingPlan{}, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap is still waiting on a progress probe the host never answered")
	}
}
