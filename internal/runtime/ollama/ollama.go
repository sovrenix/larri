// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package ollama runs a model under Ollama.
//
// Ollama is llama.cpp underneath with the weight management done for you: a
// tag instead of a file, a pull instead of a download. That convenience is the
// reason to support it, and it comes with one real cost — Ollama's server has
// no authentication and its maintainers do not plan to add any — which this
// package reports rather than hides (see SecurityNotes).
package ollama

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
)

const (
	// RemotePort is where Ollama binds on the rented host. Loopback only
	// (FR-SEC-08), and nothing publishes it.
	RemotePort = 8000

	// LogPath is where the launch redirects output.
	LogPath = "/var/log/larri-ollama.log"
)

// Runtime is the Ollama engine.
type Runtime struct{}

func New() *Runtime { return &Runtime{} }

func (r *Runtime) Kind() core.RuntimeKind { return core.RuntimeOllama }

// Requires reports the same low floor as llama.cpp, which is what Ollama runs.
func (r *Runtime) Requires() runtime.Requirements {
	return runtime.Requirements{MinComputeCapability: 500}
}

func (r *Runtime) Image(core.ModelSpec, core.SizingPlan) string {
	return "ollama/ollama:latest"
}

// SecurityNotes states the guarantee this runtime cannot give.
//
// Said plainly at bring-up rather than buried, because the operator is choosing
// between engines and this is a difference between them, not a detail.
func (r *Runtime) SecurityNotes() []string {
	return []string{
		"ollama has no server-side authentication, so this rig has no rig-side credential: " +
			"anything that reaches the server on the host can use the model. It binds loopback, " +
			"nothing publishes a port for it, and the ssh tunnel is the only way in — but a " +
			"process on the rented host needs no token. vllm enforces one.",
	}
}

var _ runtime.SecurityNoter = (*Runtime)(nil)

const findRuntimeCmd = `if command -v ollama >/dev/null 2>&1; then echo ollama; ` +
	`elif [ -x /usr/local/bin/ollama ]; then echo /usr/local/bin/ollama; ` +
	`else echo NOTFOUND; fi`

// Bootstrap starts the daemon and pulls the tag.
//
// Unlike the other runtimes, the server must be running *before* the weights
// can be fetched: `ollama pull` is a client that talks to `ollama serve`. So
// this launches the daemon and the ordering is not an accident.
func (r *Runtime) Bootstrap(ctx context.Context, sess runtime.Session,
	spec core.ModelSpec, plan core.SizingPlan, progress chan<- runtime.Progress) error {

	send := func(p runtime.Progress) {
		if progress != nil {
			select {
			case progress <- p:
			default:
			}
		}
	}

	send(runtime.Progress{Phase: "image.pull", Message: "image supplied by the provider; verifying the runtime"})
	out, err := sess.Run(ctx, findRuntimeCmd)
	if err != nil {
		return errs.Newf(errs.ClassHostFailure, "ollama.Bootstrap", "probe host: %v", err)
	}
	if strings.Contains(string(out), "NOTFOUND") {
		return errs.Newf(errs.ClassHostFailure, "ollama.Bootstrap", "no ollama on this host")
	}
	send(runtime.Progress{Phase: "image.pull", Message: "runtime found (ollama)"})

	// The daemon first, then the pull that needs it.
	_, _ = sess.Run(ctx, stopServersCmd)
	if _, err := sess.Run(ctx, serveCmd()); err != nil {
		return errs.Newf(errs.ClassHostFailure, "ollama.Bootstrap", "start daemon: %v", err)
	}
	if _, err := sess.Run(ctx, waitDaemonCmd()); err != nil {
		return errs.Newf(errs.ClassHostFailure, "ollama.Bootstrap",
			"daemon did not come up: %v", err)
	}

	send(runtime.Progress{Phase: "weights.download", Message: "ollama pull " + spec.Ref})
	if _, err := sess.Run(ctx, r.pullCmd(spec)); err != nil {
		return errs.Newf(errs.ClassHostFailure, "ollama.Bootstrap",
			"pull %s: %v", spec.Ref, err)
	}
	return nil
}

// serveCmd starts the daemon bound to loopback.
//
// OLLAMA_HOST is the only bind control Ollama offers, and it is set here rather
// than left to default because the default (0.0.0.0 in a container) would put
// an unauthenticated inference server on every interface of a machine with a
// routable address. FR-SEC-08 is not configurable, and for this engine it is
// the whole of the protection.
func serveCmd() string {
	return fmt.Sprintf(
		`export OLLAMA_HOST=%s:%d; nohup ollama serve >%s 2>&1 & sleep 1; true`,
		runtime.Loopback, RemotePort, LogPath)
}

func waitDaemonCmd() string {
	return fmt.Sprintf(
		`for i in $(seq 1 60); do `+
			`curl -fsS http://%s:%d/api/tags >/dev/null 2>&1 && exit 0; sleep 1; done; `+
			`echo "ollama did not answer on %[1]s:%[2]d" >&2; exit 1`,
		runtime.Loopback, RemotePort)
}

func (r *Runtime) pullCmd(spec core.ModelSpec) string {
	return fmt.Sprintf(`export OLLAMA_HOST=%s:%d; ollama pull %s`,
		runtime.Loopback, RemotePort, runtime.ShellQuote(spec.Ref))
}

const stopServersCmd = `pkill -f '[o]llama serve' >/dev/null 2>&1; sleep 1; true`

// Launch gives the pulled model its stable served name and returns the
// endpoint. The daemon is already running — Bootstrap needed it — so this
// aliases rather than starts.
//
// The alias is what makes FR-RT-04 hold for this engine: clients are wired to
// the served name, and changing the upstream tag must not require touching
// their config. `ollama cp` is how a tag gets a second name.
func (r *Runtime) Launch(ctx context.Context, sess runtime.Session,
	spec core.ModelSpec, plan core.SizingPlan) (runtime.Endpoint, error) {

	ep := runtime.Endpoint{
		Host: runtime.Loopback, Port: RemotePort,
		Model: spec.ServedName,
		// No Key: Ollama has no server-side credential. SecurityNotes says so
		// out loud; leaving this empty is the honest representation, and the
		// proxy simply forwards without an Authorization header.
	}
	if !ep.Valid() {
		return runtime.Endpoint{}, errs.Newf(errs.ClassModelFailure, "ollama.Launch",
			"invalid bind address %s: loopback only", ep.Host)
	}
	if spec.ServedName != "" && spec.ServedName != spec.Ref {
		cmd := fmt.Sprintf(`export OLLAMA_HOST=%s:%d; ollama cp %s %s`,
			runtime.Loopback, RemotePort,
			runtime.ShellQuote(spec.Ref), runtime.ShellQuote(spec.ServedName))
		if _, err := sess.Run(ctx, cmd); err != nil {
			return runtime.Endpoint{}, errs.Newf(errs.ClassHostFailure, "ollama.Launch",
				"alias %s as %s: %v", spec.Ref, spec.ServedName, err)
		}
	}
	return ep, nil
}

func (r *Runtime) Ready(ctx context.Context, ep runtime.Endpoint, spec core.ModelSpec) error {
	return runtime.PingReady(ctx, ep, "ollama")
}

func (r *Runtime) Logs(ctx context.Context, sess runtime.Session, tail int) (io.ReadCloser, error) {
	return runtime.TailLog(ctx, sess, LogPath, tail)
}

func (r *Runtime) Stop(ctx context.Context, sess runtime.Session) error {
	_, _ = sess.Run(ctx, stopServersCmd)
	return nil
}

const aliveCmd = `pgrep -f '[o]llama serve' >/dev/null 2>&1 && echo yes || echo no`

func (r *Runtime) Alive(ctx context.Context, sess runtime.Session) (bool, error) {
	return runtime.ProcessAlive(ctx, sess, aliveCmd, "ollama")
}

const adoptCmd = `pid=$(pgrep -f '[o]llama serve' | head -1); ` +
	`[ -z "$pid" ] && { echo NOTRUNNING; exit 0; }; ` +
	`tr '\0' '\n' < /proc/$pid/environ | grep '^OLLAMA_HOST=' || echo "OLLAMA_HOST="`

// Adopt re-attaches to a running daemon.
//
// There is no credential to recover here, so identification rests on the bind
// address instead: a daemon LARRI started is bound to loopback on the port it
// chose. That is weaker than the other runtimes' check and deliberately so —
// inventing a stronger-looking one would misrepresent what Ollama can prove.
func (r *Runtime) Adopt(ctx context.Context, sess runtime.Session,
	spec core.ModelSpec) (runtime.Endpoint, error) {

	out, err := sess.Run(ctx, adoptCmd)
	if err != nil {
		return runtime.Endpoint{}, errs.Newf(errs.ClassHostFailure, "ollama.Adopt",
			"inspect host: %v", err)
	}
	text := strings.TrimSpace(string(out))
	if strings.Contains(text, "NOTRUNNING") {
		return runtime.Endpoint{}, errs.Newf(errs.ClassHostFailure, "ollama.Adopt",
			"no server running")
	}
	host := strings.TrimPrefix(text, "OLLAMA_HOST=")
	port := RemotePort
	if i := strings.LastIndex(host, ":"); i >= 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(host[i+1:])); err == nil {
			port = n
		}
		host = host[:i]
	}
	if host != "" && host != runtime.Loopback && host != "localhost" {
		// A daemon on a routable interface is not one LARRI started, and
		// tunnelling to it would put an unauthenticated server behind a
		// recovered rig while reporting success.
		return runtime.Endpoint{}, errs.Newf(errs.ClassSecurity, "ollama.Adopt",
			"daemon bound to %s, not loopback: not started by larri", host)
	}
	return runtime.Endpoint{Host: runtime.Loopback, Port: port, Model: spec.ServedName}, nil
}

var (
	_ runtime.Runtime         = (*Runtime)(nil)
	_ runtime.Adopter         = (*Runtime)(nil)
	_ runtime.LivenessChecker = (*Runtime)(nil)
)

// LogPath is where this runtime's output is redirected, so the supervisor can
// measure growth rather than guess at progress.
func (r *Runtime) LogPath() string { return LogPath }

var _ runtime.LogWriter = (*Runtime)(nil)

// AcceptsQuant reports the schemes Ollama can load: GGUF, and nothing else.
func (r *Runtime) AcceptsQuant(quant string) bool {
	return quant == "gguf"
}

// DefaultQuantization is the balance point the GGUF ecosystem publishes most
// widely: a quarter the size of full precision, with quality loss that is
// measurable but not usually visible.
func (r *Runtime) DefaultQuantization() string { return "Q4_K_M" }
