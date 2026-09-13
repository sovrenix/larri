// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package vllm

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// bashSession runs commands in a local bash with a chosen environment, so the
// measuring command is exercised as the host runs it rather than as a string.
type bashSession struct{ env []string }

func (s bashSession) Run(ctx context.Context, cmd string) ([]byte, error) {
	c := exec.CommandContext(ctx, "bash", "-c", cmd)
	c.Env = s.env
	return c.Output()
}
func (bashSession) Dial(context.Context, int) (io.ReadWriteCloser, error) { return nil, nil }
func (bashSession) Close() error                                          { return nil }

// On RunPod the Hugging Face cache is a link onto the volume. du measures a
// link it is handed rather than what it points at, so a live run reported
// "2 MB of 2.9 GB (0%)" from start to finish of a download that completed.
func TestWeightsAreMeasuredThroughALinkedCache(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	volume := filepath.Join(dir, "workspace", "huggingface")
	if err := os.MkdirAll(volume, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(volume, "model.safetensors"), make([]byte, 3<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "cache-link")
	if err := os.Symlink(volume, link); err != nil {
		t.Fatal(err)
	}
	got, err := New().WeightsOnDisk(context.Background(),
		bashSession{env: []string{"PATH=" + os.Getenv("PATH"), "HF_HOME=" + link, "HOME=" + dir}})
	if err != nil {
		t.Fatal(err)
	}
	if got < 3<<20 {
		t.Errorf("measured %d bytes through the link; 3 MB are behind it", got)
	}
}
