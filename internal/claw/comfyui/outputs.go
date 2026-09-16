// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package comfyui

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
)

// Artifact is one file a session rendered, as it exists on the host.
type Artifact struct {
	// Path is absolute on the host.
	Path string

	// Rel is the path under the output directory, which is what it will be
	// called locally. Preserved rather than flattened, since ComfyUI's
	// SaveImage nodes write into subfolders an operator chose.
	Rel string

	Bytes   uint64
	ModTime time.Time
}

// SyncResult is what a retrieval achieved, and what it did not.
//
// Failures are counted rather than returned as a single error because the
// caller is on its way to a teardown. "Four of five images saved" is a fact an
// operator can act on; an aborted sync that saved nothing because the fifth
// file was unreadable is not.
type SyncResult struct {
	Saved   []string
	Skipped []string
	Failed  map[string]string
	Bytes   uint64
	Dir     string
}

// Complete reports whether everything on the host is now also local.
//
// This is the question teardown asks. Destroying a host is irreversible, and
// the images on it are the only thing about the rental that was not paid for
// in advance — so the answer gates the destroy rather than decorating it.
func (r SyncResult) Complete() bool { return len(r.Failed) == 0 }

// Count is how many artefacts are now held locally.
func (r SyncResult) Count() int { return len(r.Saved) + len(r.Skipped) }

// Summary is one line for the operator.
func (r SyncResult) Summary() string {
	if r.Count() == 0 && len(r.Failed) == 0 {
		return "no outputs were rendered"
	}
	s := fmt.Sprintf("%d saved", len(r.Saved))
	if len(r.Skipped) > 0 {
		s += fmt.Sprintf(", %d already local", len(r.Skipped))
	}
	if len(r.Failed) > 0 {
		s += fmt.Sprintf(", %d FAILED", len(r.Failed))
	}
	return s
}

// maxArtifactBytes bounds a single file transfer.
//
// The channel is an SSH exec whose output is buffered whole in memory and
// base64-expanded by a third on the way, so a pathological file would be paid
// for three times over in the local process. A render that exceeds this is
// reported rather than silently skipped: the operator can still reach it over
// the tunnel before the rig goes.
const maxArtifactBytes = 256 << 20

// List enumerates what the session rendered.
//
// Read from the filesystem rather than from ComfyUI's /history, and the
// difference matters at exactly the moment this is used. /history knows only
// about graphs submitted through the API in the current process; the operator
// spent the session in the browser, queueing renders LARRI never saw, and a
// teardown that saved only what LARRI submitted would destroy the rest.
func List(ctx context.Context, sess runtime.Session, dir string) ([]Artifact, error) {
	if dir == "" {
		dir = OutputDir
	}
	// -printf keeps this one round-trip instead of a stat per file. The tab
	// separator is safe where a space is not: ComfyUI filenames routinely
	// contain spaces, and splitting on whitespace loses them.
	cmd := fmt.Sprintf(
		`find %s -type f -printf '%%s\t%%T@\t%%P\n' 2>/dev/null | head -n 5000`,
		shellQuote(dir))
	out, err := sess.Run(ctx, cmd)
	if err != nil && len(out) == 0 {
		return nil, errs.Newf(errs.ClassHostFailure, "comfyui.List",
			"list outputs: %v", err)
	}
	var arts []Artifact
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		size, serr := strconv.ParseUint(strings.TrimSpace(parts[0]), 10, 64)
		if serr != nil {
			continue
		}
		secs, _ := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		rel := parts[2]
		if rel == "" {
			continue
		}
		arts = append(arts, Artifact{
			Path:    dir + "/" + rel,
			Rel:     rel,
			Bytes:   size,
			ModTime: time.Unix(int64(secs), 0),
		})
	}
	sort.Slice(arts, func(i, j int) bool { return arts[i].Rel < arts[j].Rel })
	return arts, nil
}

// SyncOptions configures retrieval.
type SyncOptions struct {
	// Dir on the host. Empty means OutputDir.
	RemoteDir string

	// Since drops artefacts older than this, so a rig adopted from a previous
	// session does not re-download what was already collected.
	Since time.Time

	// Budget bounds the whole retrieval. It exists because this runs
	// immediately before a destroy, and a rig that cannot be torn down
	// because it is busy copying files is a rig that is still billing. When
	// the budget expires, what has been saved is kept and the rest is
	// reported as failed — never silently dropped.
	Budget time.Duration
}

// Sync copies rendered artefacts to a local directory.
//
// Over SSH rather than through ComfyUI's /view, and deliberately so: this runs
// at teardown, which is frequently reached *because* something is wrong, and
// an HTTP route through a wedged ComfyUI would fail exactly when the images
// most need rescuing. SSH needs only sshd.
//
// base64 on the wire because Session.Run merges stdout and stderr into one
// buffer — a single warning on stderr would corrupt a PNG in a way that is
// invisible until the operator opens it days later.
func Sync(ctx context.Context, sess runtime.Session, localDir string, opt SyncOptions) (*SyncResult, error) {
	budget := opt.Budget
	if budget <= 0 {
		budget = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	arts, err := List(ctx, sess, opt.RemoteDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(localDir, 0o700); err != nil {
		return nil, errs.Newf(errs.ClassWiring, "comfyui.Sync",
			"create %s: %v", localDir, err)
	}
	res := &SyncResult{Failed: map[string]string{}, Dir: localDir}

	for _, a := range arts {
		if !opt.Since.IsZero() && a.ModTime.Before(opt.Since) {
			continue
		}
		dest := filepath.Join(localDir, filepath.FromSlash(a.Rel))
		if !underDir(localDir, dest) {
			// The name came off a remote filesystem, so it is not trusted to
			// stay inside the directory it is supposed to.
			res.Failed[a.Rel] = "path escapes the output directory"
			continue
		}
		if st, err := os.Stat(dest); err == nil && uint64(st.Size()) == a.Bytes {
			res.Skipped = append(res.Skipped, a.Rel)
			continue
		}
		if a.Bytes > maxArtifactBytes {
			res.Failed[a.Rel] = fmt.Sprintf("%d bytes exceeds the transfer limit", a.Bytes)
			continue
		}
		if err := fetchOne(ctx, sess, a, dest); err != nil {
			res.Failed[a.Rel] = shortErr(err)
			// A cancelled context means the budget is spent, and every
			// remaining file will fail the same way. Recording them one
			// timeout at a time would take another whole budget to do it.
			if ctx.Err() != nil {
				res.Failed[a.Rel] = "retrieval budget expired"
				break
			}
			continue
		}
		res.Saved = append(res.Saved, a.Rel)
		res.Bytes += a.Bytes
	}
	return res, nil
}

// fetchOne copies a single artefact, verifying its size before it is given its
// real name.
func fetchOne(ctx context.Context, sess runtime.Session, a Artifact, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	out, err := sess.Run(ctx, "base64 "+shellQuote(a.Path)+" 2>/dev/null")
	if err != nil && len(out) == 0 {
		return err
	}
	clean := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, string(out))
	raw, derr := base64.StdEncoding.DecodeString(clean)
	if derr != nil {
		return fmt.Errorf("decode: %w", derr)
	}
	if a.Bytes > 0 && uint64(len(raw)) != a.Bytes {
		return fmt.Errorf("size mismatch: got %d, expected %d", len(raw), a.Bytes)
	}
	// Write to a temporary name and rename, so an interrupted transfer never
	// leaves a half-written PNG that looks like a finished one.
	tmp := dest + ".part"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// underDir reports whether path stays inside root.
func underDir(root, path string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}
