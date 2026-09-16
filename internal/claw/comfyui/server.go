// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package comfyui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.sovrenix.com/larri/internal/claw/comfyui/workflow"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/sizing"
)

// DefaultImage is the base the adapter runs in.
//
// It is a **tag and not a digest**, which is a deliberate and temporary
// exception to §6.5 rather than an oversight. Pin it with
// `make refresh-image IMAGE=<ref>` before any live use, and re-derive
// MinComputeCapability and MinCUDA from the same build in the same change —
// the two are facts about one image, not free-standing numbers.
//
// The version is not arbitrary. A live run on 2.4.0 installed cleanly, fetched
// its weights, and then died importing ComfyUI: torch's infer_schema before
// 2.5 rejects PEP 585 builtin generics, so a custom op declared `stride:
// list[int]` fails to register and takes the server down with it. That failure
// arrived *after* the expensive part, which is what SmokeTest below now
// prevents — but the floor moved too, because the cheapest fix for a known-bad
// pairing is not to rent it.
const DefaultImage = "pytorch/pytorch:2.9.1-cuda12.6-cudnn9-runtime"

// DefaultComfyUIRef is the application version installed on the host.
//
// Pinned for the same reason the image is, and the pairing is the thing that
// actually has to work: `--depth 1` of a moving branch means the pair that
// succeeded yesterday is not the pair that runs today, and a bring-up that
// cannot be reproduced cannot be debugged. The operator can move it with
// --comfyui-version, and SmokeTest is what checks whatever pair they chose.
const DefaultComfyUIRef = "v0.35.2"

// Hardware floors.
//
// Read off the image above rather than from any support matrix, per §4a and
// FR-RT-16: CUDA 12.6 is what this build links against, and Volta is the safe
// architecture line for a current torch. A graph wanting bf16 wants Ampere,
// and falls back to fp16 below it rather than failing, so this is a floor
// rather than the whole story.
const (
	MinComputeCapability = 700
	MinCUDA              = 126
)

// RemotePort is where ComfyUI listens on the host's loopback interface.
const RemotePort = 8188

// Root is where the adapter installs and runs ComfyUI.
const Root = "/opt/ComfyUI"

// LogPathDefault is where the server's output lands, so a launch that failed
// before answering can still be diagnosed after the host is gone.
const LogPathDefault = "/var/log/larri-comfy.log"

// OutputDir is where rendered artefacts accumulate on the host.
const OutputDir = Root + "/output"

// Runtime is the ComfyUI adapter.
//
// It satisfies runtime.Workload and deliberately not runtime.Runtime. The
// distinction is enforced by the compiler below and is the whole point of the
// widened abstraction: everything in the lifecycle works on this, and nothing
// that expects a completion can be handed it by accident.
type Runtime struct {
	// ImageRef is the container image.
	ImageRef string

	// Graph is what this rig exists to run. It is held here for the same
	// reason vLLM holds its launcher: readiness needs it, and readiness is
	// a method on the workload rather than something the daemon composes.
	Graph *workflow.Graph

	// Bundle is the measured set of files the graph loads.
	Bundle *workflow.Bundle

	// URLs maps an asset name to where its bytes come from. Resolved before
	// the rental so an unreachable source is free to discover (§4a).
	URLs map[string]string

	// FetchStall ends a download that has stopped growing. Zero means eight
	// minutes.
	FetchStall time.Duration

	// FetchCap bounds the download even while it progresses. Zero means two
	// hours: a FLUX bundle on a mid-range link legitimately takes one.
	FetchCap time.Duration

	// PollInterval is how often the fetch is measured. Zero means fifteen
	// seconds.
	PollInterval time.Duration

	// AllowPickle records that the operator opted into code-executing
	// containers, so the disclosure can be repeated where it is acted on.
	AllowPickle bool

	// ComfyUIRef is the git ref installed on the host. Empty means
	// DefaultComfyUIRef.
	ComfyUIRef string

	hfToken secret.Secret
	python  string // discovered during bootstrap rather than assumed
	logPath string

	// readyMu guards readyPrompt.
	readyMu sync.Mutex

	// readyPrompt is the graph readiness already queued, if any.
	//
	// The readiness loop calls Ready repeatedly — that is what makes it a
	// wait rather than a single check — and for an engine a repeated call is
	// a repeated completion, which costs a few tokens. Here it would be a
	// repeated *render*: a second job queued behind the first, on a GPU
	// billed by the second, for an answer already on its way. So the
	// submission is remembered and subsequent calls resume waiting on it.
	readyPrompt string
}

var (
	_ runtime.Workload          = (*Runtime)(nil)
	_ runtime.CredentialTaker   = (*Runtime)(nil)
	_ runtime.LogWriter         = (*Runtime)(nil)
	_ runtime.LivenessChecker   = (*Runtime)(nil)
	_ runtime.SecurityNoter     = (*Runtime)(nil)
	_ runtime.WeightsProgressor = (*Runtime)(nil)
	_ runtime.FailureClassifier = (*Runtime)(nil)
)

// New builds the adapter for a graph and its measured bundle.
func New(g *workflow.Graph, b *workflow.Bundle, urls map[string]string) *Runtime {
	return &Runtime{
		ImageRef:     DefaultImage,
		ComfyUIRef:   DefaultComfyUIRef,
		Graph:        g,
		Bundle:       b,
		URLs:         urls,
		PollInterval: 15 * time.Second,
		logPath:      LogPathDefault,
	}
}

func (r *Runtime) Kind() core.RuntimeKind { return core.RuntimeComfyUI }

// Protocol reports ComfyUI's own API rather than /v1.
//
// This is the method that keeps the widening honest. A caller holding this
// workload and about to issue a chat completion can find out that it must not,
// instead of discovering it from a 404 on a rig that is already billing.
func (r *Runtime) Protocol() runtime.Protocol { return runtime.ProtocolComfyUI }

// Requires reports the hardware floors to apply during selection.
func (r *Runtime) Requires() runtime.Requirements {
	return runtime.Requirements{
		MinComputeCapability: MinComputeCapability,
		MinCUDA:              MinCUDA,
		Why:                  "the comfyui image",
		// TensorParallel stays false, and that is a statement rather than an
		// omission: ComfyUI executes a graph on one device and spreads a
		// model across cards by neither splitting nor sharding it. Selection
		// sizes against a single card's VRAM for the same reason.
		TensorParallel: false,
	}
}

// Image returns the container image.
func (r *Runtime) Image(core.ModelSpec, core.SizingPlan) string {
	if r.ImageRef == "" {
		return DefaultImage
	}
	return r.ImageRef
}

// LogPath is where the server writes, which the supervisor watches for growth.
func (r *Runtime) LogPath() string {
	if r.logPath == "" {
		return LogPathDefault
	}
	return r.logPath
}

// SetHuggingFaceToken supplies the credential for gated weights.
func (r *Runtime) SetHuggingFaceToken(t secret.Secret) { r.hfToken = t }

// SecurityNotes reports what this workload cannot uphold.
//
// ComfyUI has no server-side credential of any kind — no API key, no token, no
// setting for one. So the rig-side half of the two-credential boundary
// (§15.5.3) does not exist here, exactly as it does not for Ollama, and an
// operator deserves to know which guarantee they are getting rather than
// assuming the stronger one.
//
// What still holds is the part that matters most: the server binds loopback on
// the rented host, no port is published for it, and the SSH tunnel is the only
// route in. The local listener is still credentialled, so nothing in the
// operator's browser reaches the rig unbidden.
func (r *Runtime) SecurityNotes() []string {
	notes := []string{
		"comfyui has no server-side credential; the loopback bind and the ssh tunnel are what protect it",
		"rendered images and any prompt text are visible to the host, which has root",
	}
	if r.AllowPickle {
		notes = append(notes,
			"pickle containers were allowed: a .ckpt or .pth executes code on load, on the host holding your hugging face token")
	}
	return notes
}

// pythonCandidates are the places an image puts an interpreter that can
// import torch.
//
// A list rather than a reliance on PATH, and a live failure is why. The
// pytorch/pytorch images install into conda at /opt/conda, and set the PATH
// that finds it with a Dockerfile ENV — which applies to `docker run` and
// **not** to a command arriving over SSH. A non-interactive session sources no
// profile, so it gets the default /usr/bin:/bin PATH, where `python3` is
// either absent or a system interpreter with no torch in it. The probe
// reported "no python with torch" about an image that has one.
//
// The order matters: PATH first, so an image that has arranged its own
// environment wins, and the known layouts only as fallbacks.
var pythonCandidates = []string{
	"python3",
	"python",
	"/opt/conda/bin/python3",
	"/opt/conda/bin/python",
	"/usr/local/bin/python3",
	"/usr/bin/python3",
	"/venv/bin/python",
	"/opt/venv/bin/python",
	"/workspace/venv/bin/python",
}

// findPythonCmd locates an interpreter that can import torch, and says what it
// saw when it cannot.
//
// The diagnostic half is not decoration. The host is about to be destroyed, so
// anything not carried out in the error is lost — and "no python with torch"
// on its own gives an operator who has just paid for a rental nothing to act
// on. It cannot distinguish a missing interpreter from a present one whose
// torch import raises, which are different problems with different fixes.
func findPythonCmd() string {
	var list strings.Builder
	for _, c := range pythonCandidates {
		list.WriteString(" " + shellQuote(c))
	}
	cands := list.String()

	var b strings.Builder
	b.WriteString("for p in" + cands + "; do\n")
	b.WriteString("  command -v \"$p\" >/dev/null 2>&1 || continue\n")
	b.WriteString("  if \"$p\" -c 'import torch' >/dev/null 2>&1; then echo \"FOUND $p\"; exit 0; fi\n")
	b.WriteString("done\n")
	b.WriteString("echo NOTFOUND\n")

	// Nothing worked, so say what was there. One line naming each interpreter
	// that exists, then what importing torch in the first of them actually
	// printed — which is the difference between "no interpreter at all" and
	// "an interpreter whose torch is broken", two problems with different
	// fixes and one indistinguishable error message between them.
	b.WriteString("for p in" + cands + "; do\n")
	b.WriteString("  command -v \"$p\" >/dev/null 2>&1 || continue\n")
	b.WriteString("  echo \"saw $p\"\n")
	b.WriteString("  \"$p\" -c 'import torch' 2>&1 | tail -n 1\n")
	b.WriteString("  break\n")
	b.WriteString("done\n")
	return b.String()
}

// Bootstrap installs ComfyUI if it is absent and fetches every model the graph
// names.
//
// The download happens here rather than at launch, and that is the one real
// structural difference from the inference engines. vLLM fetches its own
// weights as it starts, so its download is observable in its log; ComfyUI
// fetches nothing and fails at the first node if a file is missing. The models
// must therefore be on disk before the server is worth starting, which makes
// this the long, expensive, billed phase — and the one that needs the stall
// detection §12.2.1 built for readiness.
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

	out, err := sess.Run(ctx, findPythonCmd())
	if err != nil && len(out) == 0 {
		return errs.Newf(errs.ClassHostFailure, "comfyui.Bootstrap",
			"could not inspect the host: %v", err)
	}
	python, saw := parsePython(string(out))
	if python == "" {
		// The image is what the operator asked the provider for, so a missing
		// torch is a configuration problem rather than a bad machine: the next
		// host runs the same image and fails identically (FR-PROV-05).
		return errs.Newf(errs.ClassModelFailure, "comfyui.Bootstrap",
			"no python with torch in image %s%s", r.Image(spec, plan), saw)
	}
	r.python = python
	send(runtime.Progress{Phase: runtime.PhaseImagePull,
		Message: "python with torch at " + python})

	if err := r.install(ctx, sess, send); err != nil {
		return err
	}
	// Between the install and the download, which is the last moment this is
	// still cheap to discover (§4a).
	if err := r.smokeTest(ctx, sess, send); err != nil {
		return err
	}
	return r.fetchModels(ctx, sess, send)
}

// install puts ComfyUI on the host if the image did not carry it.
func (r *Runtime) install(ctx context.Context, sess runtime.Session,
	send func(runtime.Progress)) error {

	check, _ := sess.Run(ctx, "test -f "+shellQuote(Root+"/main.py")+" && echo HAVE || echo MISSING")
	if strings.Contains(string(check), "HAVE") {
		send(runtime.Progress{Phase: runtime.PhaseImagePull, Percent: 100,
			Message: "comfyui already present in the image"})
		return nil
	}
	send(runtime.Progress{Phase: runtime.PhaseImagePull,
		Message: "installing comfyui"})

	// --depth 1: the history is tens of megabytes of no value on a host that
	// exists for one session and is billed by the second.
	//
	// The requirements are installed with torch held back, and that is a cost
	// control rather than tidiness. ComfyUI's requirements.txt lists torch,
	// torchvision and torchaudio; the image already carries a build matched to
	// its CUDA, and letting pip resolve them again downloads several
	// gigabytes of wheels at the rig's hourly rate to replace a working
	// install with one that may not match the driver. The image supplies the
	// runtime — the same rule the vLLM adapter follows — so pip is given
	// everything else and nothing it would fight over.
	cmd := strings.Join([]string{
		"set -e",
		"command -v git >/dev/null 2>&1 || (apt-get update -qq && apt-get install -y -qq git)",
		"command -v curl >/dev/null 2>&1 || (apt-get update -qq && apt-get install -y -qq curl)",
		"git clone --depth 1 --branch " + shellQuote(r.comfyRef()) +
			" https://github.com/comfyanonymous/ComfyUI " + shellQuote(Root),
		// torchsde and torchmetrics are deliberately not held back: they are
		// small pure-python packages the image does not carry.
		"grep -viE '^(torch|torchvision|torchaudio)([=<>!~ ]|$)' " +
			shellQuote(Root+"/requirements.txt") + " > " + shellQuote(Root+"/requirements.larri.txt"),
		r.python + " -m pip install --no-cache-dir -q -r " + shellQuote(Root+"/requirements.larri.txt"),
		"echo INSTALLED",
	}, " && ")

	out, err := sess.Run(ctx, cmd)
	if err != nil || !strings.Contains(string(out), "INSTALLED") {
		return errs.Newf(errs.ClassHostFailure, "comfyui.Bootstrap",
			"install comfyui: %v: %s", err, lastLine(string(out)))
	}
	send(runtime.Progress{Phase: runtime.PhaseImagePull, Percent: 100,
		Message: "comfyui installed"})
	return nil
}

// comfyRef is the application version to install.
func (r *Runtime) comfyRef() string {
	if r.ComfyUIRef == "" {
		return DefaultComfyUIRef
	}
	return r.ComfyUIRef
}

// smokeTestCmd imports ComfyUI's node tree without starting a server.
//
// nodes.py is the import that pulls in comfy.* and registers the custom ops,
// so it is where a torch that cannot parse a modern op signature raises. It is
// also fast and needs no weights, which is the whole point.
const smokeTestCmd = `import sys
sys.path.insert(0, %s)
import nodes
print("SMOKE_OK")`

// smokeTest proves the image's torch can actually load this ComfyUI, before
// anything expensive happens.
//
// This is §4a applied to the one precondition the adapter had been checking in
// the most costly order available. A live run installed cleanly, downloaded
// 6.5 GB of weights, launched, and only then died importing a custom op that
// torch 2.4 cannot parse — roughly fifteen billed minutes to learn something
// an import would have said in ten seconds. Worse, the failure surfaced as a
// host failure, so it was bought twice more on two more machines.
//
// Everything this needs is already on disk by now, so the check costs a
// process start. It runs between the install and the download precisely
// because that is the last moment it is still cheap.
func (r *Runtime) smokeTest(ctx context.Context, sess runtime.Session,
	send func(runtime.Progress)) error {

	send(runtime.Progress{Phase: runtime.PhaseImagePull,
		Message: "checking that comfyui loads under the image's torch"})

	script := fmt.Sprintf(smokeTestCmd, pyQuote(Root))
	cmd := fmt.Sprintf("cd %s && %s - <<'LARRI_SMOKE_EOF' 2>&1\n%s\nLARRI_SMOKE_EOF",
		shellQuote(Root), r.python, script)
	out, err := sess.Run(ctx, cmd)
	text := string(out)
	if strings.Contains(text, "SMOKE_OK") {
		return nil
	}
	// A model-class failure, not a host-class one: the next machine runs the
	// same image and the same ref and fails identically (FR-PROV-05). The
	// traceback goes in the error because the host is about to be destroyed.
	return errs.Newf(errs.ClassModelFailure, "comfyui.Bootstrap",
		"comfyui %s does not load under this image: %s",
		r.comfyRef(), pythonFault(text, err))
}

// pyQuote renders a path as a Python string literal.
func pyQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\`, `'`, `\'`).Replace(s) + "'"
}

// pythonFault pulls the useful line out of a traceback.
//
// The last non-empty line of a Python traceback is the exception and its
// message, which is the line an operator can act on. The frames above it are
// noise in a one-line error and are available in the runtime log.
func pythonFault(text string, err error) string {
	var last string
	for _, line := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			last = t
		}
	}
	if last == "" {
		if err != nil {
			return err.Error()
		}
		return "no output"
	}
	if len(last) > 300 {
		last = last[:300]
	}
	return last
}

// ClassifyFailure reads a ComfyUI traceback and says whose problem it is.
//
// waitReady cannot tell a dead host from a broken configuration, and its
// default is "try another machine". For a Python process that died on an
// import, another machine is another identical death at another rental's
// price. These signatures are the ones that mean the pairing is wrong rather
// than the hardware.
func (r *Runtime) ClassifyFailure(log string) errs.Class {
	for _, sig := range configFaults {
		if strings.Contains(log, sig) {
			return errs.ClassModelFailure
		}
	}
	return errs.ClassUnknown
}

// configFaults are log signatures that the next host reproduces exactly.
var configFaults = []string{
	"ModuleNotFoundError",
	"ImportError",
	"infer_schema",     // a torch too old for the application's custom ops
	"unsupported type", // the same, in its other wording
	"SyntaxError",
	"AttributeError: module",
	"is not compatible with",
	"requires Python",
}

// fetchModels downloads the bundle onto the host and waits for it.
//
// The wait is progress-driven and not a clock (§12.2.1). What ends it is
// *silence* — no growth in bytes on disk — rather than elapsed time, because a
// deadline that expires while a host is working throws away a partly-finished
// download and starts the same one somewhere else, at the same price, having
// kept nothing.
func (r *Runtime) fetchModels(ctx context.Context, sess runtime.Session,
	send func(runtime.Progress)) error {

	if r.Bundle == nil || len(r.Bundle.Items) == 0 {
		return errs.Newf(errs.ClassModelFailure, "comfyui.Bootstrap",
			"no models resolved for this graph")
	}

	dl := Download{Dir: ModelsDir, Log: FetchLog}
	for _, it := range r.Bundle.Items {
		url, ok := r.URLs[it.Asset.Name]
		if !ok || url == "" {
			return errs.Newf(errs.ClassModelFailure, "comfyui.Bootstrap",
				"no download url for %s", it.Asset.Name)
		}
		dl.Items = append(dl.Items, Item{
			URL: url, Kind: it.Asset.Kind, Name: it.Asset.Name, Bytes: it.Bytes,
		})
	}

	if err := WriteCredential(ctx, sess, r.hfToken); err != nil {
		return err
	}
	total := dl.TotalBytes()
	send(runtime.Progress{Phase: runtime.PhaseWeightsDownload, BytesTotal: total,
		Message: fmt.Sprintf("fetching %d model files, %s",
			len(dl.Items), sizing.HumanBytes(total))})

	if err := dl.Start(ctx, sess, !r.hfToken.Empty()); err != nil {
		return err
	}

	stall := r.FetchStall
	if stall == 0 {
		stall = 8 * time.Minute
	}
	cap := r.FetchCap
	if cap == 0 {
		cap = 2 * time.Hour
	}
	poll := r.PollInterval
	if poll <= 0 {
		poll = 15 * time.Second
	}

	var (
		last      uint64
		lastGrew  = time.Now()
		deadline  = time.Now().Add(cap)
		startedAt = time.Now()
	)
	for time.Now().Before(deadline) {
		done, err := dl.Done(ctx, sess)
		if err != nil {
			return err
		}
		if done {
			send(runtime.Progress{Phase: runtime.PhaseWeightsDownload, Percent: 100,
				BytesDone: total, BytesTotal: total,
				Message: fmt.Sprintf("models ready in %s",
					time.Since(startedAt).Round(time.Second))})
			return nil
		}

		got, gerr := dl.BytesOnDisk(ctx, sess)
		if gerr == nil {
			if got > last {
				last = got
				lastGrew = time.Now()
			}
			var pct float64
			if total > 0 {
				pct = float64(got) / float64(total) * 100
			}
			send(runtime.Progress{Phase: runtime.PhaseWeightsDownload,
				Percent: pct, BytesDone: got, BytesTotal: total,
				BytesPerSec: rate(got, time.Since(startedAt))})
		}
		// A measurement that could not run proves nothing (§4a): silence from
		// du is not evidence of a stalled download, so the clock only advances
		// on a reading that came back.
		if gerr == nil && time.Since(lastGrew) > stall {
			return errs.Newf(errs.ClassHostFailure, "comfyui.Bootstrap",
				"model fetch stalled: no bytes in %s at %s of %s",
				time.Since(lastGrew).Round(time.Second),
				sizing.HumanBytes(last), sizing.HumanBytes(total))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
	return errs.Newf(errs.ClassHostFailure, "comfyui.Bootstrap",
		"model fetch incomplete after %s at %s of %s",
		cap, sizing.HumanBytes(last), sizing.HumanBytes(total))
}

func rate(bytes uint64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(bytes) / d.Seconds()
}

// stopServersCmd clears a previous ComfyUI before a new one starts.
//
// Its own command, never combined with the launch. A single command that both
// greps for the server and starts it contains the literal target text, so the
// pattern matches the issuing shell however it is spelled — a mistake the vLLM
// path made three separate times.
const stopServersCmd = `pkill -f 'main[.]py --listen' 2>/dev/null; true`

// LaunchTimeout bounds the command that starts the server.
//
// Starting a server is a sub-second operation: the shell forks, redirects, and
// returns. There is no legitimate reason for that call to be unbounded, and a
// live run proved what unbounded costs — the command never returned, the
// tunnel was never opened, and the rig billed until the ninety-minute
// provisioning deadline would have killed it.
const LaunchTimeout = 60 * time.Second

// Launch starts ComfyUI bound to loopback.
func (r *Runtime) Launch(ctx context.Context, sess runtime.Session,
	spec core.ModelSpec, plan core.SizingPlan) (runtime.Endpoint, error) {

	ep := runtime.Endpoint{
		Host:  runtime.Loopback,
		Port:  RemotePort,
		Model: spec.ServedName,
		// No Key: ComfyUI has no server-side credential to hold one.
		// SecurityNotes says so rather than leaving it to be inferred.
	}
	// FR-SEC-08: the bind address is computed here and is not configurable. A
	// non-loopback value is rejected rather than warned about, because the
	// only reason to want one is the reason the rule exists — and on ComfyUI
	// it would publish an unauthenticated render farm on a shared public IP.
	if !ep.Valid() {
		return runtime.Endpoint{}, errs.Newf(errs.ClassModelFailure, "comfyui.Launch",
			"invalid bind address %s: loopback only", ep.Host)
	}

	// Bounded, and separately from the caller's context, so a hang here ends
	// in an error rather than in a rig that bills while nothing happens.
	lctx, cancel := context.WithTimeout(ctx, LaunchTimeout)
	defer cancel()

	_, _ = sess.Run(lctx, stopServersCmd)

	python := r.python
	if python == "" {
		python = "python3"
	}
	if err := r.startServer(lctx, sess, python); err != nil {
		return runtime.Endpoint{}, err
	}
	return ep, nil
}

// launchScriptPath is where the start script lives on the host.
const launchScriptPath = "/root/.larri-comfy-launch.sh"

// launchScript is what actually starts the server.
//
// A file rather than a one-liner, and `exec` rather than a bare call, because
// the shape matters more than the contents. See startServer.
func (r *Runtime) launchScript(python string) string {
	return fmt.Sprintf("cd %s\nexec %s main.py --listen %s --port %d "+
		"--output-directory %s --disable-auto-launch\n",
		shellQuote(Root), python, runtime.Loopback, RemotePort,
		shellQuote(OutputDir))
}

// startServer writes the start script and launches it detached.
//
// This is the fetch launcher's shape, copied deliberately rather than
// approximated, and the reason is two live failures at the same place.
//
// The first version backgrounded the server with its stdout and stderr
// redirected but stdin still on the SSH exec channel, and sshd holds a channel
// open while any live process has a descriptor on it. The command never
// returned; the rig billed for thirty-eight minutes with a working ComfyUI on
// it that nothing could reach.
//
// The second version added setsid and </dev/null — and still hung, in sixty
// seconds this time because the call was finally bounded. What it kept was the
// `cd X && ...` prefix, which makes the whole compound the asynchronous list:
// the shell forks a subshell for it, and that subshell inherits the channel's
// descriptors while only the inner command carries the redirections.
//
// The fetch launcher never had that prefix and has never hung. So the cd moves
// into a script, the async list becomes a single simple command, and the
// launcher is now byte-for-byte the shape already proven on this hardware.
// Reasoning about which of three plausible mechanisms was to blame cost two
// rentals; copying what demonstrably works costs nothing.
func (r *Runtime) startServer(ctx context.Context, sess runtime.Session, python string) error {
	write := fmt.Sprintf("cat > %s <<'LARRI_LAUNCH_EOF'\n%s\nLARRI_LAUNCH_EOF\nchmod 700 %s",
		shellQuote(launchScriptPath), r.launchScript(python), shellQuote(launchScriptPath))
	if _, err := sess.Run(ctx, write); err != nil {
		return errs.Newf(errs.ClassHostFailure, "comfyui.Launch",
			"write the start script: %v", err)
	}

	launch := fmt.Sprintf("setsid nohup sh %s </dev/null >%s 2>&1 & echo LAUNCHED",
		shellQuote(launchScriptPath), shellQuote(r.LogPath()))
	out, err := sess.Run(ctx, launch)
	if err != nil {
		return errs.Newf(errs.ClassHostFailure, "comfyui.Launch",
			"start server: %v", err)
	}
	// The marker is what proves the shell got far enough to fork and return.
	// Without it a command that produced nothing looks exactly like one that
	// worked, which is how a hang was mistaken for a launch.
	if !strings.Contains(string(out), "LAUNCHED") {
		return errs.Newf(errs.ClassHostFailure, "comfyui.Launch",
			"start server: no confirmation from the host: %s", lastLine(string(out)))
	}
	return nil
}

// Ready performs a real round-trip.
//
// For an engine this is a completion; here it is a rendered image, and the
// graph it renders is the operator's own. That is a stronger claim than a
// synthetic warm-up would make and it is the one worth making: READY then
// means "the workflow you asked for has produced a file", rather than "a
// server is listening and something unrelated worked".
//
// A UI-format graph cannot be submitted — ComfyUI's /prompt takes the API
// serialisation only — so readiness falls back to proving the server is up and
// holding a GPU. That is weaker, it is the most the format allows, and
// Caveats() says so out loud rather than letting READY quietly mean two
// different things.
func (r *Runtime) Ready(ctx context.Context, ep runtime.Endpoint, spec core.ModelSpec) error {
	c := &Client{
		Addr:  LocalAddr(ep.Host, ep.Port),
		Token: ep.Key.Reveal(),
		Probe: true,
		HTTP:  &http.Client{Timeout: 30 * time.Second},
	}
	stats, err := c.Stats(ctx)
	if err != nil {
		return err
	}
	if !stats.HasCUDA() {
		// Not a transient condition and not worth waiting out: ComfyUI has
		// already chosen its device. A CPU render on rented GPU hardware is
		// the most expensive way to produce an image there is.
		return errs.Newf(errs.ClassHostFailure, "comfyui.Ready",
			"comfyui found no cuda device")
	}
	if r.Graph == nil || !r.Graph.Format.Executable() {
		return nil
	}

	promptID, err := r.readySubmission(ctx, c)
	if err != nil {
		return err
	}
	entry, err := c.Await(ctx, promptID, 2*time.Second)
	if err != nil {
		// A graph that failed will fail again identically, so the
		// submission is forgotten and the error is returned for the
		// caller to classify. Holding it would make every later poll
		// re-report one dead render forever.
		r.forgetSubmission()
		return err
	}
	if len(entry.Files()) == 0 {
		return errs.Newf(errs.ClassModelFailure, "comfyui.Ready",
			"graph completed and produced no output")
	}
	return nil
}

// readySubmission returns the readiness render, queueing one only if none is
// outstanding.
func (r *Runtime) readySubmission(ctx context.Context, c *Client) (string, error) {
	r.readyMu.Lock()
	defer r.readyMu.Unlock()
	if r.readyPrompt != "" {
		return r.readyPrompt, nil
	}
	res, err := c.Submit(ctx, r.Graph.Raw, "larri-readiness")
	if err != nil {
		return "", err
	}
	r.readyPrompt = res.PromptID
	return res.PromptID, nil
}

func (r *Runtime) forgetSubmission() {
	r.readyMu.Lock()
	defer r.readyMu.Unlock()
	r.readyPrompt = ""
}

// Caveats reports where this rig's guarantees are weaker than usual.
func (r *Runtime) Caveats() []string {
	if r.Graph != nil && !r.Graph.Format.Executable() {
		return []string{
			"the graph is in the ui serialisation, which /prompt does not accept: " +
				"readiness proved the server and the gpu, not a render",
		}
	}
	return nil
}

// Alive reports whether the server process still exists.
//
// This is what stops LARRI paying for a decided outcome. The readiness wait is
// deliberately patient — a checkpoint legitimately loads for minutes in
// silence — and that patience is billed. It should never be spent on a process
// that has already exited.
func (r *Runtime) Alive(ctx context.Context, sess runtime.Session) (bool, error) {
	out, err := sess.Run(ctx, `pgrep -f 'main[.]py --listen' >/dev/null 2>&1 && echo ALIVE || echo GONE`)
	if err != nil && len(out) == 0 {
		return false, err
	}
	return strings.Contains(string(out), "ALIVE"), nil
}

// WeightsOnDisk reports how much of the bundle has landed.
func (r *Runtime) WeightsOnDisk(ctx context.Context, sess runtime.Session) (uint64, error) {
	return Download{Dir: ModelsDir}.BytesOnDisk(ctx, sess)
}

// Logs streams the server's output for diagnosis.
func (r *Runtime) Logs(ctx context.Context, sess runtime.Session, tail int) (io.ReadCloser, error) {
	if tail <= 0 {
		tail = 100
	}
	out, err := sess.Run(ctx, fmt.Sprintf("tail -n %d %s 2>/dev/null", tail, shellQuote(r.LogPath())))
	if err != nil && len(out) == 0 {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(out)), nil
}

// Stop halts the server. It does not stop the bill — only a destroy does —
// and conflating the two is how a rig looks torn down while it is not.
func (r *Runtime) Stop(ctx context.Context, sess runtime.Session) error {
	_, _ = sess.Run(ctx, stopServersCmd)
	return nil
}

// ModelSpecFor describes a graph in the vocabulary the rest of LARRI persists.
//
// Rig carries a ModelSpec, every surface reads it, and the journal records it,
// so a ComfyUI rig needs one even though "the model" is a set of files rather
// than a repository. The served name is what the local endpoint advertises;
// the ref names the workflow, since that is the thing an operator recognises
// when asking why a rig existed.
func ModelSpecFor(name string, g *workflow.Graph) core.ModelSpec {
	spec := core.ModelSpec{
		Ref:        name,
		Source:     core.SourceLocalPath,
		ServedName: "comfyui",
	}
	if g != nil {
		spec.ContextLen = g.Image.Pixels()
	}
	return spec
}

// parsePython reads the probe's output: the interpreter it found, or the
// evidence for why it found none.
//
// The evidence is what makes the failure actionable. A live run rented a host,
// reached this point, and reported "no python with torch in image
// pytorch/pytorch:2.4.0-cuda12.4-cudnn9-runtime" — an image that has one, in
// conda, on a PATH that a non-interactive SSH session never sees. The message
// was true about what the probe looked at and useless about what was wrong.
func parsePython(out string) (python, evidence string) {
	var saw []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if rest, ok := strings.CutPrefix(line, "FOUND "); ok {
			return strings.TrimSpace(rest), ""
		}
		if line == "NOTFOUND" || line == "" {
			continue
		}
		saw = append(saw, line)
	}
	if len(saw) > 0 {
		evidence = ": " + strings.Join(saw, "; ")
	}
	return "", evidence
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
}
