// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package runpod

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.sovrenix.com/larri/internal/buildinfo"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/provider"
)

// logsURL is on a different host from the pod API.
const logsURL = "https://api.runpod.io/v2/pods/%s/logs?tail=%d"

// BootLog returns what the pod has said, oldest first.
//
// This is the progress signal RunPod's status field cannot give. Its
// desiredStatus is RUNNING from creation onwards; the log, by contrast,
// narrates properly — "create container", "Pulling from", "Status: Image is up
// to date", "start container: begin", and then whatever the container itself
// writes.
//
// It reads a backfill and stops. The endpoint is Server-Sent Events and would
// otherwise stay open forever, but LARRI polls rather than subscribes: the
// supervisor already has a loop, and a long-lived stream would be a second
// thing to keep alive across the exact failures this is meant to observe.
func (p *Provider) BootLog(ctx context.Context, instanceID string, tail int) ([]string, error) {
	if p.c.Key.Empty() {
		return nil, errs.Newf(errs.ClassModelFailure, "runpod.BootLog", "no api key")
	}
	if tail <= 0 {
		tail = 100
	}
	base := p.c.LogsURL
	if base == "" {
		base = logsURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf(base, instanceID, tail), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.c.Key.Reveal())
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", buildinfo.UserAgent())

	// Its own deadline: the stream never ends on its own, so the read has to.
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req = req.WithContext(rctx)

	cl := p.c.HTTP
	if cl == nil {
		cl = http.DefaultClient
	}
	resp, err := cl.Do(req)
	if err != nil {
		// A timeout here is the expected end of a stream that does not end,
		// not a failure — whatever arrived before it is still the answer.
		if rctx.Err() != nil {
			return nil, nil
		}
		return nil, errs.Newf(errs.ClassProviderTransient, "runpod.BootLog", "%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errs.Newf(errs.ClassProviderTransient, "runpod.BootLog",
			"http %d", resp.StatusCode)
	}
	return parseSSE(resp.Body, tail), nil
}

// parseSSE pulls the log lines out of a Server-Sent Events stream.
//
// Only the data: frames matter, and each carries one JSON object. A frame that
// does not parse is skipped rather than failing the read: this is a progress
// signal, and a signal that refused to work because one line was malformed
// would be worse than one that missed a line.
func parseSSE(r interface{ Read([]byte) (int, error) }, max int) []string {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var out []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var ev struct {
			Source string `json:"source"`
			Line   string `json:"line"`
			TS     string `json:"ts"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &ev); err != nil {
			continue
		}
		if ev.Line == "" {
			continue
		}
		// The source is kept because it separates the provider's account of
		// the boot from the container's own, and during a bring-up those are
		// different phases rather than one noisy stream.
		out = append(out, ev.Source+": "+ev.Line)
		if len(out) >= max {
			break
		}
	}
	return out
}

var _ provider.BootLogger = (*Provider)(nil)
