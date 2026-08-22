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
	"encoding/json"
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
const DefaultImage = "vllm/vllm-openai:latest"

// RemotePort is where vLLM listens on the host's loopback interface.
const RemotePort = 8000

// containerName is fixed so logs and teardown can find the process without
// bookkeeping that a crash could lose.
const containerName = "larri-vllm"

// Runtime is the vLLM adapter.
type Runtime struct {
	// ImageRef is the container image. Named to leave Image() free for the
	// interface method.
	ImageRef string

	// Progress reporting granularity for weight download.
	PollInterval time.Duration
}

var _ runtime.Runtime = (*Runtime)(nil)

// New builds the adapter.
func New() *Runtime {
	return &Runtime{ImageRef: DefaultImage, PollInterval: 5 * time.Second}
}

func (r *Runtime) Kind() core.RuntimeKind { return core.RuntimeVLLM }

// Image returns the container image for this spec and plan.
func (r *Runtime) Image(core.ModelSpec, core.SizingPlan) string {
	if r.ImageRef == "" {
		return DefaultImage
	}
	return r.ImageRef
}

// Bootstrap pulls the image and the weights.
//
// Weights are the one genuinely large download and the operator is paying for
// every second of it, so progress is reported in bytes rather than as a phase
// that could be mistaken for a hang (FR-RT-06).
func (r *Runtime) Bootstrap(ctx context.Context, sess runtime.Session,
	spec core.ModelSpec, plan core.SizingPlan, progress chan<- runtime.Progress) error {

	send := func(p runtime.Progress) {
		if progress == nil {
			return
		}
		select {
		case progress <- p:
		case <-ctx.Done():
		default: // never block bootstrap on a slow consumer
		}
	}

	send(runtime.Progress{Phase: runtime.PhaseImagePull, Message: r.Image(spec, plan)})
	if _, err := sess.Run(ctx, "docker pull "+shellQuote(r.Image(spec, plan))); err != nil {
		return errs.Newf(errs.ClassHostFailure, "vllm.Bootstrap", "image pull: %v", err)
	}
	send(runtime.Progress{Phase: runtime.PhaseImagePull, Percent: 100})

	// vLLM downloads weights itself on first launch, into the HF cache. Doing
	// it as a separate step would double the disk usage for no benefit, so
	// the download is observed during launch rather than performed here.
	send(runtime.Progress{
		Phase:   runtime.PhaseWeightsDownload,
		Message: "weights download begins at launch and is reported from the container log",
	})
	return nil
}

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

	cmd := r.launchCommand(spec, plan, ep)
	if _, err := sess.Run(ctx, cmd); err != nil {
		return runtime.Endpoint{}, errs.Newf(errs.ClassHostFailure, "vllm.Launch",
			"start container: %v", err)
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
	// reference an operator pasted. Only the values are quoted, which keeps
	// the command readable in logs while still neutralising the half that
	// could carry an injection.
	type flag struct{ name, value string }
	flags := []flag{
		{"--model", spec.Ref},
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
		flags = append(flags, flag{"--tensor-parallel-size",
			strconv.Itoa(plan.TensorParallelSize)})
	}
	if spec.Quantization != "" && !isUnquantised(spec.Quantization) {
		flags = append(flags, flag{"--quantization", spec.Quantization})
	}
	// §6.6: tool calling is a launch-time property. A runtime started without
	// it accepts tools[] and answers in prose, which looks like a bad model
	// rather than a missing flag.
	toolCalling := spec.ToolCalling != core.Forbid && spec.ToolParser != ""
	if toolCalling {
		flags = append(flags, flag{"--tool-call-parser", spec.ToolParser})
	}

	var b strings.Builder
	b.WriteString("docker rm -f " + containerName + " >/dev/null 2>&1; ")
	b.WriteString("docker run -d --name " + containerName + " --gpus all --restart no ")
	// Docker's -p defaults to every interface. On a host with a routable
	// address that would publish an unauthenticated inference server to
	// anyone who scans for it, so the bind address is explicit on both sides.
	b.WriteString(fmt.Sprintf("-p %s:%d:%d ", runtime.Loopback, RemotePort, RemotePort))
	b.WriteString("-v /root/.cache/huggingface:/root/.cache/huggingface ")
	b.WriteString("-e HF_TOKEN ") // inherited from the session, never written to disk
	b.WriteString(shellQuote(r.Image(spec, plan)))
	for _, f := range flags {
		b.WriteString(" " + f.name + " " + shellQuote(f.value))
	}
	if toolCalling {
		b.WriteString(" --enable-auto-tool-choice")
	}
	return b.String()
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
	body, err := json.Marshal(map[string]any{
		"model":      ep.Model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
	})
	if err != nil {
		return err
	}
	req := ChatRequest{Endpoint: ep, Body: body}
	resp, err := req.Do(ctx)
	if err != nil {
		return errs.Newf(errs.ClassHostFailure, "vllm.Ready", "%v", err)
	}
	if len(resp.Choices) == 0 {
		return errs.Newf(errs.ClassHostFailure, "vllm.Ready",
			"completion returned no choices")
	}
	return nil
}

// Logs streams the container log.
func (r *Runtime) Logs(ctx context.Context, sess runtime.Session, tail int) (io.ReadCloser, error) {
	n := "all"
	if tail > 0 {
		n = strconv.Itoa(tail)
	}
	s, ok := sess.(interface {
		Stream(context.Context, string) (io.ReadCloser, error)
	})
	if !ok {
		out, err := sess.Run(ctx, fmt.Sprintf("docker logs --tail %s %s 2>&1", n, containerName))
		if err != nil {
			return nil, err
		}
		return io.NopCloser(strings.NewReader(string(out))), nil
	}
	return s.Stream(ctx, fmt.Sprintf("docker logs -f --tail %s %s 2>&1", n, containerName))
}

// Stop halts the container.
func (r *Runtime) Stop(ctx context.Context, sess runtime.Session) error {
	if _, err := sess.Run(ctx, "docker rm -f "+containerName); err != nil {
		return errs.Newf(errs.ClassHostFailure, "vllm.Stop", "%v", err)
	}
	return nil
}

// shellQuote makes a value safe as a single shell word.
//
// Model references and served names reach a remote shell, and a ref
// containing a quote or a semicolon would otherwise be command injection on a
// machine LARRI has root on.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Requires reports vLLM's hardware floor.
//
// vLLM's CUDA kernels need Volta or newer. A Pascal card will pull the image,
// download the weights, and fail at engine init — after the operator has paid
// for all of it. Checking during selection turns twenty minutes and a bill
// into a line of output.
func (r *Runtime) Requires() runtime.Requirements {
	return runtime.Requirements{
		MinComputeCapability: 700,
		Why:                  "vLLM",
	}
}
