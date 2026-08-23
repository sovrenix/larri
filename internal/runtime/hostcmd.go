// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"go.sovrenix.com/larri/internal/errs"
)

// ShellQuote wraps a value for safe use in a remote shell command.
//
// Flag *names* are literals LARRI controls; flag *values* can come from a model
// reference an operator pasted, so only the values go through here. Every
// runtime builds command lines the same way for the same reason, which is why
// this is shared rather than reimplemented per engine.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TailLog reads the last n lines of a runtime's log file.
//
// This is the only window into a launch that failed before the server ever
// answered — an OOM at load, a missing weight file, an unsupported quantisation
// — so it reads the file the launch redirected into rather than questioning a
// process that may no longer exist.
func TailLog(ctx context.Context, sess Session, path string, tail int) (io.ReadCloser, error) {
	n := "200"
	if tail > 0 {
		n = strconv.Itoa(tail)
	}
	cmd := fmt.Sprintf("tail -n %s %s 2>/dev/null || echo '(no runtime log yet)'", n, path)
	if st, ok := sess.(interface {
		Stream(context.Context, string) (io.ReadCloser, error)
	}); ok {
		return st.Stream(ctx, cmd)
	}
	out, err := sess.Run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(string(out))), nil
}

// ArgValue reads a flag's value out of a process argv, accepting both
// `--flag value` and `--flag=value`.
//
// Adoption depends on this: after a restart the rig credential is recovered
// from the running server's own command line, and a runtime that understood
// only one spelling would fail against an image that used the other.
func ArgValue(argv []string, flag string) string {
	for i, a := range argv {
		a = strings.TrimSpace(a)
		if a == flag && i+1 < len(argv) {
			return strings.TrimSpace(argv[i+1])
		}
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// PingReady performs the readiness check every OpenAI-compatible runtime
// performs: a real completion, round-tripped.
//
// Shared because all three engines had it byte-identical apart from the label
// in the error. That is the wrong kind of duplication — not three
// implementations of one idea, but one implementation written three times,
// where a fix to the readiness contract would have to be made in three places
// and could be made in two.
//
// The engine-specific part is only the name in the error, which is why that is
// the only parameter.
func PingReady(ctx context.Context, ep Endpoint, engine string) error {
	body, err := json.Marshal(map[string]any{
		"model":      ep.Model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
	})
	if err != nil {
		return err
	}
	resp, err := ChatRequest{Endpoint: ep, Body: body}.Do(ctx)
	if err != nil {
		return errs.Newf(errs.ClassHostFailure, engine+".Ready", "%v", err)
	}
	// NFR-05: READY means a completion came back, not that a port answered.
	if len(resp.Choices) == 0 {
		return errs.Newf(errs.ClassHostFailure, engine+".Ready",
			"completion returned no choices")
	}
	return nil
}

// ProcessAlive reports whether a process matching cmd's pattern is running.
//
// cmd must print "yes" or "no" and must not match the shell that issues it —
// a lesson this codebase has learned three times (see the bracket convention
// in each runtime's aliveCmd).
func ProcessAlive(ctx context.Context, sess Session, cmd, engine string) (bool, error) {
	out, err := sess.Run(ctx, cmd)
	if err != nil {
		return false, errs.Newf(errs.ClassHostFailure, engine+".Alive", "probe host: %v", err)
	}
	return strings.Contains(string(out), "yes"), nil
}
