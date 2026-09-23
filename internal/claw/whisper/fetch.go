// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package whisper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/sizing"
)

// The model is pulled by the *host* rather than relayed through the operator's
// link. Three gigabytes on a datacentre link that ranking already selected for
// (§4b) beats the same bytes crossing a domestic upstream twice while the rig
// sits idle and billing.
//
// Fetching it explicitly rather than letting the server download it on first
// request is what makes the wait observable. A lazy download happens inside a
// request that has not answered yet, which is indistinguishable from a wedged
// server — and the whole point of FR-RT-15 is that a wait ends on silence
// rather than on a clock, which needs something to measure.

const (
	fetchLog        = "/var/log/larri-whisper-fetch.log"
	fetchDonePrefix = "/var/log/larri-whisper-fetch."
	fetchDoneSuffix = ".done"
	fetchScriptPath = "/root/.larri-whisper-fetch.sh"
)

// doneMarker names the model it completed.
//
// A fixed path could not: a host reused for a second model — an adopt, a
// resume, a change of --model on a rig already up — would find the previous
// fetch's marker and read it as proof that *this* one had finished, and
// bootstrap would proceed against weights that were never pulled. The digest
// rather than the name, because a repository id contains slashes.
func doneMarker(model string) string {
	sum := sha256.Sum256([]byte(model))
	return fetchDonePrefix + hex.EncodeToString(sum[:8]) + fetchDoneSuffix
}

// fetchScript downloads the model repository into the host's cache.
//
// The marker is written only after the download returns successfully, and its
// existence is the completion signal. A detached process reports its exit
// status nowhere, so without a marker "still running" and "failed ten minutes
// ago" look identical to anything watching from outside.
func (r *Runtime) fetchScript() string {
	var b strings.Builder
	b.WriteString("set -e\n")
	if !r.hfToken.Empty() {
		// In the script rather than on a command line: an argument is
		// visible in the process table and in whatever LARRI logs about the
		// command it built (invariant 9). The file is chmod 700.
		fmt.Fprintf(&b, "export HF_TOKEN=%s\n", shellQuote(r.hfToken.Reveal()))
		fmt.Fprintf(&b, "export HUGGING_FACE_HUB_TOKEN=%s\n", shellQuote(r.hfToken.Reveal()))
	}
	fmt.Fprintf(&b, "export HF_HOME=%s\n", shellQuote(CacheRoot))
	fmt.Fprintf(&b, "export HF_HUB_CACHE=%s\n", shellQuote(BlobCache))
	python := r.launch.Python
	if python == "" {
		python = "python3"
	}
	// snapshot_download rather than the CLI, because the CLI is a separate
	// package that this image may not carry while huggingface_hub is a hard
	// dependency of the server itself.
	fmt.Fprintf(&b, "%s - <<'LARRI_FETCH_PY'\n", shellQuote(python))
	b.WriteString("from huggingface_hub import snapshot_download\n")
	// cache_dir explicitly, rather than trusting the environment to resolve to
	// the directory the gauge watches. The two have to be the same path by
	// construction: when they were not, a healthy download and a wedged one
	// measured identically, and the stall detector reads that measurement.
	fmt.Fprintf(&b, "p = snapshot_download(%q, cache_dir=%q)\n", r.model(), BlobCache)
	b.WriteString("print('DOWNLOADED', p)\n")
	b.WriteString("LARRI_FETCH_PY\n")
	fmt.Fprintf(&b, "touch %s\n", shellQuote(doneMarker(r.model())))
	return b.String()
}

// startFetch writes the script and launches it detached.
//
// The same shape as the launcher, and for the same reason: sshd holds an exec
// channel open while any live process has a descriptor on it, so a foreground
// download would hang the call that started it rather than return.
func (r *Runtime) startFetch(ctx context.Context, sess runtime.Session) error {
	// Checked, not ignored. A marker this failed to clear is read moments
	// later as proof that the fetch finished, and bootstrap then proceeds
	// against weights that are still arriving.
	if _, err := sess.Run(ctx, "rm -f "+shellQuote(doneMarker(r.model()))); err != nil {
		return errs.Newf(errs.ClassHostFailure, "whisper.Bootstrap",
			"clear the fetch marker: %v", err)
	}

	write := fmt.Sprintf("cat > %s <<'LARRI_FETCH_EOF'\n%s\nLARRI_FETCH_EOF\nchmod 700 %s",
		shellQuote(fetchScriptPath), r.fetchScript(), shellQuote(fetchScriptPath))
	if _, err := sess.Run(ctx, write); err != nil {
		return errs.Newf(errs.ClassHostFailure, "whisper.Bootstrap",
			"write the fetch script: %v", err)
	}
	start := fmt.Sprintf("setsid nohup sh %s </dev/null >%s 2>&1 & echo STARTED",
		shellQuote(fetchScriptPath), shellQuote(fetchLog))
	out, err := sess.Run(ctx, start)
	if err != nil {
		return errs.Newf(errs.ClassHostFailure, "whisper.Bootstrap",
			"start the fetch: %v", err)
	}
	if !strings.Contains(string(out), "STARTED") {
		return errs.Newf(errs.ClassHostFailure, "whisper.Bootstrap",
			"start the fetch: no confirmation from the host: %s", lastLine(string(out)))
	}
	return nil
}

// fetchFinished reports whether the marker is present.
func (r *Runtime) fetchFinished(ctx context.Context, sess runtime.Session) (bool, error) {
	out, err := sess.Run(ctx,
		"test -f "+shellQuote(doneMarker(r.model()))+" && echo DONE || echo WORKING")
	if err != nil && len(out) == 0 {
		return false, errs.Newf(errs.ClassHostFailure, "whisper.Bootstrap",
			"check the fetch: %v", err)
	}
	return strings.Contains(string(out), "DONE"), nil
}

// bytesOnDisk measures the cache, which is what progress is driven by.
func (r *Runtime) bytesOnDisk(ctx context.Context, sess runtime.Session) uint64 {
	out, err := sess.Run(ctx, "du -sb "+shellQuote(BlobCache)+" 2>/dev/null | cut -f1")
	if err != nil && len(out) == 0 {
		return 0
	}
	n, perr := strconv.ParseUint(strings.TrimSpace(lastLine(string(out))), 10, 64)
	if perr != nil {
		return 0
	}
	return n
}

// fetchModel downloads the model and watches it arrive.
//
// The wait ends on *silence* rather than on a clock (FR-RT-15). A deadline
// that expires while a host is still pulling throws away a partly-finished
// download and starts the same one somewhere else, which is how three separate
// bugs on this project turned a slow link into a failed rental.
func (r *Runtime) fetchModel(ctx context.Context, sess runtime.Session,
	send func(runtime.Progress)) error {

	total := r.ModelBytes
	msg := "fetching " + r.model()
	if total > 0 {
		msg += ", " + sizing.HumanBytes(total)
	}
	send(runtime.Progress{Phase: runtime.PhaseWeightsDownload,
		BytesTotal: total, Message: msg})

	if err := r.startFetch(ctx, sess); err != nil {
		return err
	}

	stall := r.FetchStall
	if stall == 0 {
		stall = 8 * time.Minute
	}
	poll := r.PollInterval
	if poll <= 0 {
		poll = 15 * time.Second
	}

	var (
		last     uint64
		lastGrew = time.Now()
		started  = time.Now()
	)
	for {
		done, err := r.fetchFinished(ctx, sess)
		if err != nil {
			return err
		}
		if done {
			onDisk := r.bytesOnDisk(ctx, sess)
			msg := "model ready"
			// The marker is the authority — it runs only under `set -e` after
			// a successful download — so a disagreement with the gauge is a
			// fault in the *gauge*, and saying so is the point. A live run
			// reported "100% (29 B of 1.4 GB)" and nothing complained, which
			// meant the stall detector reading the same number could not have
			// told a wedged download from a working one either.
			if total > 0 && onDisk < total/2 {
				msg = fmt.Sprintf(
					"model ready, but only %s is visible in %s of an expected %s: "+
						"the progress measurement is looking in the wrong place",
					sizing.HumanBytes(onDisk), BlobCache, sizing.HumanBytes(total))
			}
			send(runtime.Progress{Phase: runtime.PhaseWeightsDownload, Percent: 100,
				BytesDone: onDisk, BytesTotal: total, Message: msg})
			return nil
		}

		now := r.bytesOnDisk(ctx, sess)
		if now > last {
			last = now
			lastGrew = time.Now()
		}
		p := runtime.Progress{Phase: runtime.PhaseWeightsDownload,
			BytesDone: now, BytesTotal: total}
		if elapsed := time.Since(started).Seconds(); elapsed > 0 {
			p.BytesPerSec = float64(now) / elapsed
		}
		if total > 0 {
			p.Percent = clampPercent(float64(now) / float64(total) * 100)
		}
		send(p)

		if time.Since(lastGrew) > stall {
			// Nothing has arrived for long enough that the host is the
			// problem rather than the link. Host-class, so the next machine
			// is worth trying.
			tail, _ := sess.Run(ctx, "tail -n 5 "+shellQuote(fetchLog)+" 2>/dev/null")
			return errs.Newf(errs.ClassHostFailure, "whisper.Bootstrap",
				"the download stopped growing at %s: %s",
				sizing.HumanBytes(now), lastLine(string(tail)))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 99 {
		// Never 100 before the marker: a cache that happens to match the
		// expected size is not a finished download.
		return 99
	}
	return v
}
