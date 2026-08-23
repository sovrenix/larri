// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package runtime

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
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
