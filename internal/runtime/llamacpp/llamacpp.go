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
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/sizing"
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
	launcher   string        // discovered server command
	weights    Weights       // resolved weight file and its size, from ResolveGGUF
	hfToken    secret.Secret // weight-download credential, never persisted
	hfEndpoint string        // a mirror, when the host cannot reach huggingface.co
}

// SetWeights records what ResolveGGUF chose.
//
// Resolution happens locally, before anything is rented, so a repository that
// lacks the requested quantisation costs a line of output rather than a paid
// download that fails at the end of it.
func (r *Runtime) SetWeights(w Weights) { r.weights = w }

// SetGGUF records a weight file whose size is not known.
func (r *Runtime) SetGGUF(file string) { r.weights = Weights{File: file} }

// ResolveWeights picks the GGUF to fetch and records it for Bootstrap.
//
// The default quantisation is applied here, before the listing is read,
// because this is the only place that knows it is needed. A ref naming a file
// gets no default: the file already says what it carries, and a default
// would contradict any file that is not Q4_K_M.
func (r *Runtime) ResolveWeights(ctx context.Context, spec core.ModelSpec) (runtime.Weights, error) {
	quant := spec.Quantization
	if quant == "" && explicitFile(spec.Ref) == "" {
		quant = r.DefaultQuantization()
	}
	w, err := ResolveGGUF(ctx, spec.Ref, spec.Revision, quant, r.hfToken)
	if err != nil {
		return runtime.Weights{}, err
	}
	r.weights = w
	return w, nil
}

var _ runtime.WeightResolver = (*Runtime)(nil)

// WeightBytes is the measured size of this model's weights, or zero when the
// repository did not publish one.
//
// Sizing prefers it to its own estimate, which is a parameter count times a
// table of average bits per weight — two approximations, and wrong in the
// direction that OOMs whenever a publisher packs a quantisation differently
// than its name implies.
func (r *Runtime) WeightBytes() uint64 { return r.weights.Bytes }

var _ runtime.WeightSizer = (*Runtime)(nil)

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
	if err := r.checkFreeSpace(ctx, sess); err != nil {
		return err
	}
	// A large model is published in parts, and every part has to arrive. The
	// engine finds the rest for itself once it holds the first, but it cannot
	// find what was never fetched.
	shards := ShardFiles(file)
	for i, sh := range shards {
		msg := "fetching " + localName(sh)
		if len(shards) > 1 {
			msg = fmt.Sprintf("fetching %s (part %d of %d)", localName(sh), i+1, len(shards))
		}
		send(runtime.Progress{Phase: "weights.download", Message: msg})
		stop := r.reportDownload(ctx, sess, send)
		_, err := sess.Run(ctx, r.downloadCmd(spec, sh))
		stop()
		if err != nil {
			return errs.Newf(errs.ClassHostFailure, "llamacpp.Bootstrap",
				"download weights: %v", err)
		}
	}
	return nil
}

// downloadPollInterval is how often the weights directory is measured while a
// shard is in flight. Each sample is one SSH exec against a handful of files,
// so it costs a round trip; often enough that a stalled pull is visible,
// rarely enough that it is not the thing making noise.
var downloadPollInterval = 10 * time.Second

// reportDownload publishes download progress until the returned stop is
// called.
//
// FR-RT-06 asks that a multi-GB download not look like a hang, and for this
// engine it did: the operator saw one "fetching" line and then nothing for
// three quarters of an hour. vLLM answers WeightsOnDisk and the daemon samples
// it during the readiness wait — but llama.cpp fetches earlier, inside
// Bootstrap, with a blocking curl, so nothing was watching.
//
// The whole directory is measured rather than the current file: shards
// already fetched are bytes already paid for, and the total covers all of
// them. A size that could not be measured publishes nothing; a progress line
// is not worth inventing (§4a). Each Run opens its own SSH session, so this
// rides the same connection as the download it is watching.
func (r *Runtime) reportDownload(ctx context.Context, sess runtime.Session,
	send func(runtime.Progress)) func() {

	total := r.weights.Bytes
	if total == 0 {
		return func() {}
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(downloadPollInterval)
		defer t.Stop()
		var last uint64
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			got, err := r.WeightsOnDisk(ctx, sess)
			if err != nil || got == 0 || got == last {
				continue
			}
			last = got
			// Measured on the host, so bounded here: more than the whole
			// download is other files in the directory, or a host making
			// things up, and neither is progress.
			if got > total {
				got = total
			}
			send(runtime.Progress{
				Phase: "weights.download", Percent: float64(got) / float64(total) * 100,
				BytesDone: got, BytesTotal: total,
			})
		}
	}()
	return func() { close(done); <-stopped }
}

// WeightsOnDisk reports how many bytes of the model have arrived.
//
// du over the weights directory: a handful of large files, so it costs a stat
// each. Errors are the caller's to ignore — a directory that does not exist
// yet is zero bytes, not a failure.
//
// The apparent size, not the blocks allocated. A filesystem that reserves
// space ahead of a growing file — XFS does, in doubling steps — made a live
// download read "96% (52.0 GB of 53.9 GB)" while its first 27.8 GB part was
// still arriving, then fall back to 87% when the file closed and the
// reservation was trimmed. The bytes written are what has arrived.
func (r *Runtime) WeightsOnDisk(ctx context.Context, sess runtime.Session) (uint64, error) {
	out, err := sess.Run(ctx, "du -sb "+shellQuote(ModelDir)+" 2>/dev/null | cut -f1")
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
}

var _ runtime.WeightsProgressor = (*Runtime)(nil)

// freeSpaceCmd reports, in KiB, what is free where the weights land and what
// is already there. One round trip, and `df -P` so the column layout is fixed.
var freeSpaceCmd = "mkdir -p " + shellQuote(ModelDir) + " && " +
	"df -Pk " + shellQuote(ModelDir) + " | awk 'NR==2{print $4}' && " +
	"du -sk " + shellQuote(ModelDir) + " 2>/dev/null | cut -f1"

// checkFreeSpace refuses to start a download the disk cannot hold.
//
// The disk was sized to the weights before renting, but sizing it and the
// weights landing on it are two claims. On RunPod the second depends on a
// link the start script makes to the volume, and that link is allowed to fail
// so a pod is never stranded before sshd. This is what catches it: without
// the check, a missing link was discovered twenty gigabytes into a download,
// every one of them billed.
//
// Host-attributable, so the next offer is tried. A check that cannot run
// proves nothing, and neither does an unmeasured weight size (§4a).
func (r *Runtime) checkFreeSpace(ctx context.Context, sess runtime.Session) error {
	need := r.weights.Bytes
	if need == 0 {
		return nil
	}
	out, err := sess.Run(ctx, freeSpaceCmd)
	if err != nil {
		return nil
	}
	f := strings.Fields(string(out))
	if len(f) < 2 {
		return nil
	}
	freeKiB, err1 := strconv.ParseUint(f[0], 10, 64)
	haveKiB, err2 := strconv.ParseUint(f[1], 10, 64)
	if err1 != nil || err2 != nil {
		return nil
	}
	// What is already there counts: a resumed download pays only for the rest.
	if (freeKiB+haveKiB)*1024 >= need {
		return nil
	}
	return errs.Newf(errs.ClassHostFailure, "llamacpp.Bootstrap",
		"disk %s free under %s cannot hold %s of weights",
		sizing.HumanBytes(freeKiB*1024), ModelDir, sizing.HumanBytes(need))
}

// downloadCmd fetches one GGUF file.
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
	if r.hfEndpoint != "" {
		auth += fmt.Sprintf("export HF_ENDPOINT=%s; ", shellQuote(r.hfEndpoint))
	}
	// The URL keeps the repository's path; the destination does not. See
	// localName: a quantisation published in a directory would otherwise be
	// written into a directory that was never created.
	dest := ModelDir + "/" + localName(file)
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
		{"-m", ModelDir + "/" + localName(file)},
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
	// The binary and its shared objects ship in the same directory, and a
	// non-interactive ssh session inherits none of the image's ENV — so
	// /app/llama-server exits immediately with "error while loading shared
	// libraries: libllama-server-impl.so". Setting the search path to the
	// launcher's own directory is what makes the image's layout work off its
	// entrypoint.
	if dir := filepath.Dir(r.launcher); dir != "." && dir != "/" {
		b.WriteString("export LD_LIBRARY_PATH=")
		b.WriteString(runtime.ShellQuote(dir))
		b.WriteString("${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}; ")
	}
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
	return runtime.PingReady(ctx, ep, "llamacpp")
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
	return runtime.ProcessAlive(ctx, sess, aliveCmd, "llamacpp")
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
	if r.weights.File != "" {
		return r.weights.File, nil
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

// LogPath is where this runtime's output is redirected, so the supervisor can
// measure growth rather than guess at progress.
func (r *Runtime) LogPath() string { return LogPath }

var _ runtime.LogWriter = (*Runtime)(nil)

// AcceptsQuant reports the schemes llama.cpp can load: GGUF, and nothing else.
func (r *Runtime) AcceptsQuant(quant string) bool {
	return quant == "gguf"
}

// DefaultQuantization is the balance point the GGUF ecosystem publishes most
// widely: a quarter the size of full precision, with quality loss that is
// measurable but not usually visible.
func (r *Runtime) DefaultQuantization() string { return "Q4_K_M" }

// SetHuggingFaceEndpoint points weight downloads at a mirror.
func (r *Runtime) SetHuggingFaceEndpoint(endpoint string) { r.hfEndpoint = endpoint }
