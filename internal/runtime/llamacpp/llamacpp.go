// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package llamacpp runs a GGUF model under llama.cpp's server.
//
// It differs from vLLM in the one way that matters to selection: llama.cpp can
// spill layers to CPU, so it *survives* under-provisioned VRAM at a throughput
// cost rather than failing to start. That makes it the fallback when a model
// does not fit (§6.3), and it is why its hardware floor is far lower.
package llamacpp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/secret"
)

const (
	// RemotePort is where the server binds on the rented host. Loopback only
	// (FR-SEC-08); nothing publishes it.
	RemotePort = 8000

	// LogPath is where the launch redirects output. A launch that fails
	// before the server answers leaves this as the only account of why.
	LogPath = "/var/log/larri-llamacpp.log"

	// ModelDir is where the GGUF lands.
	ModelDir = "/root/.larri/models"
)

// Runtime is the llama.cpp engine.
type Runtime struct {
	launcher string        // discovered server command
	gguf     string        // resolved weight file, from ResolveGGUF
	hfToken  secret.Secret // weight-download credential, never persisted
}

// SetGGUF records the file ResolveGGUF chose.
//
// Resolution happens locally, before anything is rented, so a repository that
// lacks the requested quantisation costs a line of output rather than a paid
// download that fails at the end of it.
func (r *Runtime) SetGGUF(file string) { r.gguf = file }

func New() *Runtime { return &Runtime{} }

func (r *Runtime) Kind() core.RuntimeKind { return core.RuntimeLlamaCpp }

// Requires reports a much lower floor than vLLM's.
//
// llama.cpp's CUDA backend targets Maxwell and later, and it can offload to
// CPU besides — so the cards vLLM rejects are exactly the cheap ones this
// engine exists to make usable. Setting the floor at vLLM's would discard the
// reason to have a second runtime at all.
func (r *Runtime) Requires() runtime.Requirements {
	return runtime.Requirements{MinComputeCapability: 500}
}

// Image returns the container image. M1/M3 use a stock image; the digest-pinned
// matrix (§6.5) follows.
func (r *Runtime) Image(core.ModelSpec, core.SizingPlan) string {
	return "ghcr.io/ggml-org/llama.cpp:server-cuda"
}

// SetHuggingFaceToken takes the weight-download credential at launch, so it
// never reaches a snapshot or a journal entry (FR-STATE-05).
func (r *Runtime) SetHuggingFaceToken(t secret.Secret) { r.hfToken = t }

var _ runtime.CredentialTaker = (*Runtime)(nil)

// findRuntimeCmd locates the server binary, which images name inconsistently:
// upstream renamed `server` to `llama-server`, and some images ship both.
const findRuntimeCmd = `if command -v llama-server >/dev/null 2>&1; then echo llama-server; ` +
	`elif command -v server >/dev/null 2>&1; then echo server; ` +
	`elif [ -x /app/llama-server ]; then echo /app/llama-server; ` +
	`else echo NOTFOUND; fi`

// Bootstrap verifies the engine and downloads the GGUF.
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
		return errs.Newf(errs.ClassHostFailure, "llamacpp.Bootstrap", "probe host: %v", err)
	}
	launcher := strings.TrimSpace(string(out))
	if launcher == "" || strings.Contains(launcher, "NOTFOUND") {
		return errs.Newf(errs.ClassHostFailure, "llamacpp.Bootstrap",
			"no llama.cpp server on this host")
	}
	r.launcher = launcher
	send(runtime.Progress{Phase: "image.pull", Message: "runtime found (" + launcher + ")"})

	file, err := r.weightFile(spec)
	if err != nil {
		return err
	}
	send(runtime.Progress{Phase: "weights.download", Message: "fetching " + file})
	if _, err := sess.Run(ctx, r.downloadCmd(spec, file)); err != nil {
		return errs.Newf(errs.ClassHostFailure, "llamacpp.Bootstrap",
			"download weights: %v", err)
	}
	return nil
}

// downloadCmd fetches the single GGUF file.
//
// The token goes into the environment rather than the command line: argv is
// world-readable through /proc on a machine whose operator is not you, and a
// weight-download credential is one of the few secrets here that is *not*
// already theirs (§15.4).
func (r *Runtime) downloadCmd(spec core.ModelSpec, file string) string {
	url := fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s",
		RepoOf(spec.Ref), revisionOr(spec, "main"), file)
	auth := ""
	if !r.hfToken.Empty() {
		auth = fmt.Sprintf("export HF_TOKEN=%s; ", shellQuote(r.hfToken.Reveal()))
	}
	dest := ModelDir + "/" + file
	return auth +
		fmt.Sprintf("mkdir -p %s && ", shellQuote(ModelDir)) +
		// -C - resumes a partial file, so a retried bootstrap does not pay
		// for the same gigabytes twice.
		fmt.Sprintf(`curl -fSL --retry 3 -C - -o %s `, shellQuote(dest)) +
		`${HF_TOKEN:+-H "Authorization: Bearer $HF_TOKEN"} ` +
		shellQuote(url)
}

// stopServersCmd is issued as its OWN command. A single command that greps for
// the server and then starts it contains the literal target text, so the
// pattern matches the issuing shell however it is spelled.
const stopServersCmd = `pkill -f '[l]lama-server' >/dev/null 2>&1; ` +
	`pkill -f '[/]app/server' >/dev/null 2>&1; sleep 1; true`

// Launch starts the server bound to loopback.
func (r *Runtime) Launch(ctx context.Context, sess runtime.Session,
	spec core.ModelSpec, plan core.SizingPlan) (runtime.Endpoint, error) {

	key, err := secret.Generate(32)
	if err != nil {
		return runtime.Endpoint{}, err
	}
	ep := runtime.Endpoint{
		Host: runtime.Loopback, Port: RemotePort,
		Model: spec.ServedName, Key: key,
	}
	if !ep.Valid() {
		return runtime.Endpoint{}, errs.Newf(errs.ClassModelFailure, "llamacpp.Launch",
			"invalid bind address %s: loopback only", ep.Host)
	}
	if r.launcher == "" {
		r.launcher = "llama-server"
	}
	_, _ = sess.Run(ctx, stopServersCmd)

	cmd, err := r.launchCommand(spec, plan, ep)
	if err != nil {
		return runtime.Endpoint{}, err
	}
	if _, err := sess.Run(ctx, cmd); err != nil {
		return runtime.Endpoint{}, errs.Newf(errs.ClassHostFailure, "llamacpp.Launch",
			"start server: %v", err)
	}
	return ep, nil
}

func (r *Runtime) launchCommand(spec core.ModelSpec, plan core.SizingPlan,
	ep runtime.Endpoint) (string, error) {

	file, err := r.weightFile(spec)
	if err != nil {
		return "", err
	}
	type flag struct{ name, value string }
	flags := []flag{
		{"--host", runtime.Loopback},
		{"--port", strconv.Itoa(RemotePort)},
		{"-m", ModelDir + "/" + file},
		{"--alias", spec.ServedName},
		{"--api-key", ep.Key.Reveal()},
	}
	if plan.ContextLen > 0 {
		flags = append(flags, flag{"-c", strconv.Itoa(plan.ContextLen)})
	}
	// -ngl is what makes this engine survive a model that does not fit: it is
	// the number of layers on the GPU, and the rest run on CPU.
	if plan.OffloadLayers > 0 {
		flags = append(flags, flag{"-ngl", strconv.Itoa(plan.OffloadLayers)})
	} else {
		flags = append(flags, flag{"-ngl", "999"}) // all layers, when it fits
	}

	var b strings.Builder
	b.WriteString("nohup ")
	b.WriteString(r.launcher)
	for _, f := range flags {
		b.WriteString(" ")
		b.WriteString(f.name)
		b.WriteString(" ")
		b.WriteString(shellQuote(f.value))
	}
	// Tool calling is template-driven here rather than parser-driven: --jinja
	// makes the server use the model's own chat template, and the parser is
	// inferred from it (§6.2).
	if spec.ToolCalling != core.Forbid {
		b.WriteString(" --jinja")
	}
	b.WriteString(" >")
	b.WriteString(LogPath)
	b.WriteString(" 2>&1 &")
	return b.String(), nil
}

// Ready performs a real completion round-trip (NFR-05).
func (r *Runtime) Ready(ctx context.Context, ep runtime.Endpoint, spec core.ModelSpec) error {
	body, err := json.Marshal(map[string]any{
		"model":      ep.Model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
	})
	if err != nil {
		return err
	}
	resp, err := runtime.ChatRequest{Endpoint: ep, Body: body}.Do(ctx)
	if err != nil {
		return errs.Newf(errs.ClassHostFailure, "llamacpp.Ready", "%v", err)
	}
	if len(resp.Choices) == 0 {
		return errs.Newf(errs.ClassHostFailure, "llamacpp.Ready",
			"completion returned no choices")
	}
	return nil
}

func (r *Runtime) Logs(ctx context.Context, sess runtime.Session, tail int) (io.ReadCloser, error) {
	return runtime.TailLog(ctx, sess, LogPath, tail)
}

func (r *Runtime) Stop(ctx context.Context, sess runtime.Session) error {
	_, _ = sess.Run(ctx, stopServersCmd)
	return nil
}

const aliveCmd = `pgrep -f '[l]lama-server' >/dev/null 2>&1 && { echo yes; exit 0; }; ` +
	`pgrep -f '[/]app/server' >/dev/null 2>&1 && { echo yes; exit 0; }; echo no`

// Alive reports whether the server process still exists (FR-RT-13).
func (r *Runtime) Alive(ctx context.Context, sess runtime.Session) (bool, error) {
	out, err := sess.Run(ctx, aliveCmd)
	if err != nil {
		return false, errs.Newf(errs.ClassHostFailure, "llamacpp.Alive", "probe host: %v", err)
	}
	return strings.Contains(string(out), "yes"), nil
}

const adoptCmd = `pid=$(pgrep -f '[l]lama-server' | head -1); ` +
	`[ -z "$pid" ] && pid=$(pgrep -f '[/]app/server' | head -1); ` +
	`[ -z "$pid" ] && { echo NOTRUNNING; exit 0; }; ` +
	`tr '\0' '\n' < /proc/$pid/cmdline`

// Adopt re-attaches to a server this host is already running (FR-SUP-13).
func (r *Runtime) Adopt(ctx context.Context, sess runtime.Session,
	spec core.ModelSpec) (runtime.Endpoint, error) {

	out, err := sess.Run(ctx, adoptCmd)
	if err != nil {
		return runtime.Endpoint{}, errs.Newf(errs.ClassHostFailure, "llamacpp.Adopt",
			"inspect host: %v", err)
	}
	text := string(out)
	if strings.Contains(text, "NOTRUNNING") {
		return runtime.Endpoint{}, errs.Newf(errs.ClassHostFailure, "llamacpp.Adopt",
			"no server running")
	}
	argv := strings.Split(strings.TrimSpace(text), "\n")
	key := runtime.ArgValue(argv, "--api-key")
	if key == "" {
		return runtime.Endpoint{}, errs.Newf(errs.ClassHostFailure, "llamacpp.Adopt",
			"server has no api key: not started by larri")
	}
	port := RemotePort
	if p := runtime.ArgValue(argv, "--port"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return runtime.Endpoint{}, errs.Newf(errs.ClassHostFailure, "llamacpp.Adopt",
				"unreadable port %q", p)
		}
		port = n
	}
	served := spec.ServedName
	if s := runtime.ArgValue(argv, "--alias"); s != "" {
		served = s
	}
	ep := runtime.Endpoint{Host: runtime.Loopback, Port: port, Model: served, Key: secret.New(key)}
	if !ep.Valid() {
		return runtime.Endpoint{}, errs.Newf(errs.ClassModelFailure, "llamacpp.Adopt",
			"invalid bind address %s: loopback only", ep.Host)
	}
	return ep, nil
}

var (
	_ runtime.Runtime         = (*Runtime)(nil)
	_ runtime.Adopter         = (*Runtime)(nil)
	_ runtime.LivenessChecker = (*Runtime)(nil)
)

// weightFile returns the resolved GGUF, falling back to a ref that named one
// outright.
func (r *Runtime) weightFile(spec core.ModelSpec) (string, error) {
	if r.gguf != "" {
		return r.gguf, nil
	}
	return GGUFFile(spec)
}

func revisionOr(spec core.ModelSpec, def string) string {
	if spec.Revision != "" {
		return spec.Revision
	}
	return def
}

func shellQuote(s string) string { return runtime.ShellQuote(s) }
