// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package vllm runs vLLM on a rented host.
//
// Everything vLLM-specific lives here (P2). Above this package nothing knows
// which engine serves the endpoint, and the four things that differ between
// engines — how weights arrive, how VRAM fit is computed, what "ready" means,
// and whether tool calling is enabled at launch — are all resolved inside.
package vllm

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/secret"
)

// DefaultImage is the stock image M1 uses.
//
// Q-07 ratified pre-baked, digest-pinned images and §6.5 stands, but building
// and signing an image matrix is a separate workstream. M1 therefore exercises
// the stock-image fallback that §6.5 already requires — which has the useful
// side effect that the fallback is tested by the milestone depending on it,
// rather than rotting unused until the day it is needed.
// DefaultImage is pinned by digest, not by tag.
//
// The hardware floors below are read off this exact image — its
// TORCH_CUDA_ARCH_LIST and CUDA_VERSION decide which GPUs can run it. A
// moving tag silently invalidates them: `latest` dropped Volta from its arch
// list, the floor still said 7.0, and LARRI rented three V100 boxes that
// could never have loaded a kernel.
//
// Refresh deliberately with `make refresh-image`, which re-reads both values
// from the registry and updates them together. TestPinnedImageMatchesFloors
// checks the pair against the live registry when asked.
//
// linux/amd64 manifest of vllm/vllm-openai:latest as of 2026-08-26.
const DefaultImage = "vllm/vllm-openai@sha256:2286e8533ca8b6bc777594bae30524f1426ba46ca21797524e06df6a94b06635"

// ImageArchList and ImageCUDA are what the pinned image declares. They exist
// so the floors and the check have one source rather than two.
const (
	ImageArchList = "7.5 8.0 8.6 8.9 9.0 10.0 12.0"
	ImageCUDA     = "13.0.2"
)

// RemotePort is where vLLM listens on the host's loopback interface.
const RemotePort = 8000

// Runtime is the vLLM adapter.
type Runtime struct {
	// ImageRef is the container image. Named to leave Image() free for the
	// interface method.
	ImageRef string

	// launcher is how vLLM starts on this host, discovered during bootstrap
	// rather than assumed, because images package it differently.
	launcher string

	// hfToken authenticates weight downloads for gated repositories. Held
	// here rather than on ModelSpec because the spec is persisted and this
	// must not be (FR-STATE-05).
	hfToken secret.Secret

	// Progress reporting granularity for weight download.
	PollInterval time.Duration
}

var _ runtime.Runtime = (*Runtime)(nil)

// New builds the adapter.
func New() *Runtime {
	return &Runtime{ImageRef: DefaultImage, PollInterval: 5 * time.Second}
}

func (r *Runtime) Kind() core.RuntimeKind { return core.RuntimeVLLM }

// SetHuggingFaceToken supplies the credential for gated weights.
func (r *Runtime) SetHuggingFaceToken(t secret.Secret) { r.hfToken = t }

// Image returns the container image for this spec and plan.
func (r *Runtime) Image(core.ModelSpec, core.SizingPlan) string {
	if r.ImageRef == "" {
		return DefaultImage
	}
	return r.ImageRef
}

// LogPath is where the server's output lands on the host, so a launch that
// failed before answering can still be diagnosed.
const LogPath = "/var/log/larri-vllm.log"

// Bootstrap verifies the runtime is present and usable.
//
// It does NOT pull an image, and the obvious design being wrong here is worth
// recording. A Vast instance **is** the container: the image named in
// CreateSpec is what the instance runs, onstart executes inside it, and SSH
// connects into it. Running `docker pull` from there is docker-in-docker
// inside a container with no docker daemon, which failed every live bring-up
// with `docker: command not found` while reading like a network problem.
//
// The image therefore arrives with the instance, and bootstrap's job is to
// confirm what turned up. Weights are still fetched by vLLM at launch, where
// their progress is observable in the log.
func (r *Runtime) Bootstrap(ctx context.Context, sess runtime.Session,
	spec core.ModelSpec, plan core.SizingPlan, progress chan<- runtime.Progress) error {

	send := func(p runtime.Progress) {
		if progress == nil {
			return
		}
		select {
		case progress <- p:
		default:
		}
	}
	send(runtime.Progress{Phase: runtime.PhaseImagePull,
		Message: "image supplied by the provider; verifying the runtime"})

	out, err := sess.Run(ctx, findRuntimeCmd)
	if err != nil && len(out) == 0 {
		return errs.Newf(errs.ClassHostFailure, "vllm.Bootstrap",
			"could not inspect the host: %v", err)
	}
	launcher := strings.TrimSpace(string(out))
	if launcher == "" || strings.Contains(launcher, "NOTFOUND") {
		// The image is what the operator asked the provider for, so a missing
		// runtime is a configuration problem rather than a bad machine: the
		// next host runs the same image and fails identically (FR-PROV-05).
		return errs.Newf(errs.ClassModelFailure, "vllm.Bootstrap",
			"no vllm entrypoint in image %s", r.Image(spec, plan))
	}
	r.launcher = launcher
	send(runtime.Progress{Phase: runtime.PhaseImagePull, Percent: 100,
		Message: "runtime found (" + launcher + ")"})
	send(runtime.Progress{Phase: runtime.PhaseWeightsDownload,
		Message: "weights are fetched at launch; progress appears in the runtime log"})
	return nil
}

// findRuntimeCmd locates a way to start vLLM, because images package it
// differently and assuming one shape is how a bring-up fails on a host that
// was fine.
const findRuntimeCmd = `if command -v vllm >/dev/null 2>&1; then echo "vllm serve"; ` +
	`elif python3 -c "import vllm" >/dev/null 2>&1; then echo "python3 -m vllm.entrypoints.openai.api_server --model"; ` +
	`elif python -c "import vllm" >/dev/null 2>&1; then echo "python -m vllm.entrypoints.openai.api_server --model"; ` +
	`else echo NOTFOUND; fi`

// Launch starts vLLM bound to loopback and returns its endpoint.
func (r *Runtime) Launch(ctx context.Context, sess runtime.Session,
	spec core.ModelSpec, plan core.SizingPlan) (runtime.Endpoint, error) {

	key, err := secret.Generate(32)
	if err != nil {
		return runtime.Endpoint{}, err
	}
	ep := runtime.Endpoint{
		Host:  runtime.Loopback,
		Port:  RemotePort,
		Model: spec.ServedName,
		Key:   key,
	}
	// FR-SEC-08: the bind address is computed here and is not configurable.
	// A non-loopback value is rejected rather than warned about, because the
	// only reason to want one is the reason the rule exists.
	if !ep.Valid() {
		return runtime.Endpoint{}, errs.Newf(errs.ClassModelFailure, "vllm.Launch",
			"invalid bind address %s: loopback only", ep.Host)
	}

	// Stop first, in its OWN command. Combining the two is what made the
	// bracket insufficient: a single command that greps for `vllm serve` and
	// then launches it contains the literal target text, so the pattern
	// matches the issuing shell however it is spelled. Two commands, and
	// neither can kill the other's shell.
	//
	// A cleanup that finds nothing to kill exits non-zero, which is the normal
	// case on a fresh host rather than a failure.
	_, _ = sess.Run(ctx, stopServersCmd)

	if _, err := sess.Run(ctx, r.launchCommand(spec, plan, ep)); err != nil {
		return runtime.Endpoint{}, errs.Newf(errs.ClassHostFailure, "vllm.Launch",
			"start server: %v", err)
	}
	return ep, nil
}

// launchCommand builds the docker invocation.
//
// The container publishes to 127.0.0.1 on the host, not to 0.0.0.0. Docker's
// -p defaults to every interface, which on a machine with a routable address
// would expose an unauthenticated inference server to anyone who scans for it
// — so the bind address is written explicitly on both sides of the mapping.
func (r *Runtime) launchCommand(spec core.ModelSpec, plan core.SizingPlan, ep runtime.Endpoint) string {
	// Flag names are literals LARRI controls; values may come from a model
	// reference an operator pasted. Only the values are quoted.
	type flag struct{ name, value string }
	flags := []flag{
		{"--host", runtime.Loopback},
		{"--port", strconv.Itoa(RemotePort)},
		{"--served-model-name", spec.ServedName},
		{"--api-key", ep.Key.Reveal()},
	}
	if plan.ContextLen > 0 {
		flags = append(flags, flag{"--max-model-len", strconv.Itoa(plan.ContextLen)})
	}
	if plan.GPUMemUtilization > 0 {
		flags = append(flags, flag{"--gpu-memory-utilization",
			strconv.FormatFloat(plan.GPUMemUtilization, 'f', 2, 64)})
	}
	if plan.TensorParallelSize > 1 {
		flags = append(flags, flag{"--tensor-parallel-size", strconv.Itoa(plan.TensorParallelSize)})
	}
	if spec.Quantization != "" && !isUnquantised(spec.Quantization) {
		flags = append(flags, flag{"--quantization", spec.Quantization})
	}
	// Volta and Turing have no bfloat16, and vLLM takes its dtype from the
	// model config, where almost every current release says bfloat16. Left
	// alone it loads 50-odd GB of weights and only then refuses, so the
	// override is written at launch on any pre-Ampere card.
	if needsHalfOverride(plan.ComputeCapability, spec.Quantization) {
		flags = append(flags, flag{"--dtype", "float16"})
	}
	toolCalling := spec.ToolCalling != core.Forbid && spec.ToolParser != ""
	if toolCalling {
		flags = append(flags, flag{"--tool-call-parser", spec.ToolParser})
	}

	launcher, modelFlag := r.launcher, ""
	if launcher == "" {
		launcher = "vllm serve"
	}
	if rest, ok := strings.CutSuffix(launcher, " --model"); ok {
		launcher, modelFlag = rest, " --model"
	}

	var b strings.Builder
	// Idempotent: a re-launch must not leave two servers fighting for the GPU.

	b.WriteString(": > " + LogPath + "; ")
	// Exported into the shell rather than prefixed onto the command, so the
	// token does not appear in the server's own argv. FR-RT-03: it reaches the
	// host as a process environment variable and is never written to a file
	// there. The host has root and can read /proc/<pid>/environ regardless
	// (§15.7) — this keeps it out of casual view, not out of the operator's
	// threat model.
	if !r.hfToken.Empty() {
		b.WriteString("export HF_TOKEN=" + shellQuote(r.hfToken.Reveal()) + "; ")
		b.WriteString("export HUGGING_FACE_HUB_TOKEN=\"$HF_TOKEN\"; ")
	}
	// The server must outlive this exec channel. Without nohup and a detached
	// redirect it dies with the SSH session and readiness chases a process
	// that was never going to be there.
	b.WriteString("nohup " + launcher + modelFlag + " " + shellQuote(spec.Ref))
	for _, f := range flags {
		b.WriteString(" " + f.name + " " + shellQuote(f.value))
	}
	if toolCalling {
		b.WriteString(" --enable-auto-tool-choice")
	}
	fmt.Fprintf(&b, " >%s 2>&1 & echo started", LogPath)
	return b.String()
}

// needsHalfOverride reports whether the launch has to name float16 itself.
//
// Only 16-bit weights are affected: a quantised checkpoint carries its own
// storage type and vLLM computes in whatever the kernel supports. Capability
// 0 means the hardware never answered, and the flag is left off rather than
// forced onto a card that may well be an Ampere.
func needsHalfOverride(computeCapability int, quant string) bool {
	const ampere = 800
	return computeCapability > 0 && computeCapability < ampere && isUnquantised(quant)
}

func isUnquantised(q string) bool {
	switch strings.ToLower(q) {
	case "fp16", "f16", "float16", "bf16", "bfloat16", "fp32", "f32", "":
		return true
	}
	return false
}

// Ready performs a real completion round-trip.
//
// NFR-05: READY means a completion has come back. A TCP connect proves a
// process is listening and a 200 on /health proves it can answer a trivial
// question about itself; neither proves the model loaded, fits in VRAM, or
// will produce a token. The distinction matters because the failure this
// catches — a server that accepts connections and never completes — is
// exactly what an under-sized rig looks like.
func (r *Runtime) Ready(ctx context.Context, ep runtime.Endpoint, spec core.ModelSpec) error {
	return runtime.PingReady(ctx, ep, "vllm")
}

// Logs streams the runtime log.
//
// This is the only window into a launch that failed before the server ever
// answered — an OOM at load, a missing weight file, an unsupported
// quantisation — so it reads the file the launch redirected into rather than
// questioning a process that may no longer exist.
func (r *Runtime) Logs(ctx context.Context, sess runtime.Session, tail int) (io.ReadCloser, error) {
	return runtime.TailLog(ctx, sess, LogPath, tail)
}

// stopServersCmd kills any running server without killing the shell that
// issues it.
//
// Two things are needed and the first alone is not enough. The bracket makes
// the pattern a regex matching `vllm serve` while the literal `[v]llm serve`
// in this command's own argv does not match it. But that only holds while the
// command contains nothing else naming the binary — and a command that killed
// and then launched contained the real `vllm serve` too, so the pattern
// matched its own shell regardless of spelling. Hence this is issued
// separately from the launch.
//
// `pkill -f` matches the full command line of every process, including the
// shell running this very command — whose argv contains the pattern, because
// the launch that follows names the same binary. A plain `pkill -f 'vllm
// serve'` therefore terminates its own parent, which a live run reported as
//
//	Process exited with status 143 from signal TERM
//
// The bracket makes the pattern a regex that matches `vllm serve` while the
// literal text `[v]llm serve` sitting in the shell's argv does not match it.
// It is the standard fix for a check that cannot otherwise distinguish itself
// from its subject.
const stopServersCmd = `pkill -f '[v]llm serve' >/dev/null 2>&1; ` +
	`pkill -f '[v]llm\.entrypoints\.openai' >/dev/null 2>&1; sleep 1; true`

// Stop halts the server.
func (r *Runtime) Stop(ctx context.Context, sess runtime.Session) error {
	_, err := sess.Run(ctx, stopServersCmd)
	if err != nil {
		return errs.Newf(errs.ClassHostFailure, "vllm.Stop", "%v", err)
	}
	return nil
}

// shellQuote makes a value safe as a single shell word.
//
// Model references and served names reach a remote shell, and a ref
// containing a quote or a semicolon would otherwise be command injection on a
// machine LARRI has root on.
func shellQuote(s string) string { return runtime.ShellQuote(s) }

// Requires reports vLLM's hardware floor.
//
// vLLM's CUDA kernels need Volta or newer. A Pascal card will pull the image,
// download the weights, and fail at engine init — after the operator has paid
// for all of it. Checking during selection turns twenty minutes and a bill
// into a line of output.
func (r *Runtime) Requires() runtime.Requirements {
	// Derived from the pinned image, never written down twice. Both floors
	// are facts about one specific build, and the way they went wrong before
	// was drifting apart from it: the floor said 7.0 from vLLM's historical
	// support matrix while the image had dropped Volta from its arch list,
	// so V100 boxes — the cheapest hardware with enough VRAM for a 27B model
	// — passed selection and could never have loaded a kernel.
	return runtime.Requirements{
		MinComputeCapability: lowestArch(ImageArchList),
		MinCUDA:              cudaTimesTen(ImageCUDA),
		Why:                  "vLLM",
	}
}

// lowestArch returns the least compute capability the image has kernels for,
// times 100. That is what the hardware must clear — not what the engine
// documents, and not what it supported in an earlier release.
func lowestArch(archList string) int {
	low := 0
	for _, f := range strings.Fields(archList) {
		f = strings.TrimSuffix(f, "+PTX")
		maj, min, ok := strings.Cut(f, ".")
		if !ok {
			continue
		}
		m, err := strconv.Atoi(maj)
		if err != nil {
			continue
		}
		n, err := strconv.Atoi(min)
		if err != nil {
			continue
		}
		if v := m*100 + n*10; low == 0 || v < low {
			low = v
		}
	}
	return low
}

// cudaTimesTen turns "13.0.2" into 130.
func cudaTimesTen(v string) int {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return 0
	}
	maj, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0
	}
	min, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0
	}
	return maj*10 + min
}

// adoptCmd prints the argv of a running vLLM server, one element per line.
//
// The bracket in the pattern keeps the search from matching the shell that
// issues it — a lesson learned the expensive way, when a self-matching pattern
// made a process appear to be running that was in fact the query for it.
const adoptCmd = `pid=$(pgrep -f '[v]llm serve' | head -1); ` +
	`[ -z "$pid" ] && pid=$(pgrep -f '[v]llm\.entrypoints\.openai' | head -1); ` +
	`[ -z "$pid" ] && { echo NOTRUNNING; exit 0; }; ` +
	`tr '\0' '\n' < /proc/$pid/cmdline`

// Adopt re-attaches to a vLLM server this host is already running.
//
// The port and the rig credential are read back from the process's own argv,
// which is where Launch put them. Nothing is restarted, so the weights stay
// resident and recovery costs a round-trip rather than a reload.
func (r *Runtime) Adopt(ctx context.Context, sess runtime.Session,
	spec core.ModelSpec) (runtime.Endpoint, error) {

	out, err := sess.Run(ctx, adoptCmd)
	if err != nil {
		return runtime.Endpoint{}, errs.Newf(errs.ClassHostFailure, "vllm.Adopt",
			"inspect host: %v", err)
	}
	text := string(out)
	if strings.Contains(text, "NOTRUNNING") {
		return runtime.Endpoint{}, errs.Newf(errs.ClassHostFailure, "vllm.Adopt",
			"no server running")
	}

	argv := strings.Split(strings.TrimSpace(text), "\n")
	value := func(flag string) string { return runtime.ArgValue(argv, flag) }

	key := value("--api-key")
	if key == "" {
		// A server running without the credential LARRI issues is not one
		// LARRI started. Adopting it would put an unauthenticated endpoint
		// behind a tunnel and call it recovered.
		return runtime.Endpoint{}, errs.Newf(errs.ClassHostFailure, "vllm.Adopt",
			"server has no api key: not started by larri")
	}
	port := RemotePort
	if p := value("--port"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return runtime.Endpoint{}, errs.Newf(errs.ClassHostFailure, "vllm.Adopt",
				"unreadable port %q", p)
		}
		port = n
	}
	served := spec.ServedName
	if s := value("--served-model-name"); s != "" {
		served = s
	}

	ep := runtime.Endpoint{
		Host:  runtime.Loopback,
		Port:  port,
		Model: served,
		Key:   secret.New(key),
	}
	if !ep.Valid() {
		return runtime.Endpoint{}, errs.Newf(errs.ClassModelFailure, "vllm.Adopt",
			"invalid bind address %s: loopback only", ep.Host)
	}
	return ep, nil
}

var _ runtime.Adopter = (*Runtime)(nil)

// aliveCmd reports whether a vLLM server process exists. The bracket keeps the
// pattern from matching the shell that runs it.
const aliveCmd = `pgrep -f '[v]llm serve' >/dev/null 2>&1 && { echo yes; exit 0; }; ` +
	`pgrep -f '[v]llm\.entrypoints\.openai' >/dev/null 2>&1 && { echo yes; exit 0; }; echo no`

// Alive reports whether the server process is still running.
func (r *Runtime) Alive(ctx context.Context, sess runtime.Session) (bool, error) {
	return runtime.ProcessAlive(ctx, sess, aliveCmd, "vllm")
}

var _ runtime.LivenessChecker = (*Runtime)(nil)

// LogPath is where this runtime's output is redirected, so the supervisor can
// measure growth rather than guess at progress.
func (r *Runtime) LogPath() string { return LogPath }

var _ runtime.LogWriter = (*Runtime)(nil)

// AcceptsQuant reports the schemes vLLM can load.
//
// GGUF is excluded deliberately: vLLM's support for it is partial and slow,
// and the engine exists here for the cases llama.cpp cannot serve. MLX is
// Apple-only.
func (r *Runtime) AcceptsQuant(quant string) bool {
	switch quant {
	case "awq", "gptq", "int4", "int8", "fp8", "nvfp4", "mxfp4":
		return true
	}
	return false
}
