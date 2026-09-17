// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package whisper is the speech-to-text claw: a transcription server run on a
// rented GPU while the operator's own application stays on their machine.
//
// It is a *local* claw (§6.8.1). Nothing is produced on the host, so there is
// nothing to collect — but a client's configuration was changed here and is
// put back before the instance goes.
//
// What it stands up is a runtime.Workload and deliberately not a
// runtime.Runtime. The surface is OpenAI-shaped — POST /v1/audio/transcriptions
// with a multipart file, GET /v1/models — which is compatibility with the
// client ecosystem and not with the chat contract. Nothing may issue a
// completion against it, and runtime.ProtocolOpenAIAudio is how a caller finds
// that out before renting rather than from a 404.
package whisper

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/secret"
)

const (
	// DefaultImage carries the server, its Python, CTranslate2 and cuDNN.
	//
	// One pin rather than ComfyUI's two, and that is the substantive
	// difference between these claws. ComfyUI is a git checkout installed
	// into a torch image, so the image and the revision have to move together
	// and a pair nobody has run fails after six gigabytes of weights. Here
	// the image *is* the server, so there is no second half to disagree with
	// it.
	DefaultImage = "fedirz/faster-whisper-server:latest-cuda"

	// DefaultModel is the CTranslate2 conversion of whisper large-v3.
	DefaultModel = "Systran/faster-whisper-large-v3"

	// DefaultComputeType keeps the weights at float16.
	//
	// int8_float16 halves what the model occupies at rest and saves about
	// 1.6 GB of a 6.6 GB requirement — real money at the cheap end of the
	// market, and not the half it looks like, because CTranslate2 still
	// computes in float16 either way (see sizing.SpeechRequest).
	DefaultComputeType = "float16"

	// RemotePort is where the server listens on the host's loopback.
	RemotePort = 8000

	// MinComputeCapability is Volta. CTranslate2's float16 path wants tensor
	// cores; below this it runs, slowly, in a way that makes the rental
	// pointless.
	MinComputeCapability = 700

	// MinCUDA is the runtime the image is built against, times ten, read from
	// the image's own config rather than from anybody's documentation
	// (§4a): CUDA_VERSION=12.2.2, with NVIDIA_REQUIRE_CUDA asking for
	// driver>=525.
	MinCUDA = 122

	// LogPathDefault is where the server writes, so a launch that failed
	// before answering can still be diagnosed after the host is gone.
	LogPathDefault = "/var/log/larri-whisper.log"

	// CacheRoot is where the model lands, and what the fetch measures.
	CacheRoot = "/root/.cache/huggingface"

	// UVProject is where the image keeps its uv project, and the directory the
	// server must run from.
	//
	// Not a guess: the image's config says WorkingDir=/root/faster-whisper-server
	// and Cmd=["uv","run","uvicorn","--factory","faster_whisper_server.main:create_app"].
	// A first paid run failed here because the probe looked only on PATH, and
	// a uv project's interpreter lives in .venv where a non-interactive SSH
	// session never sees it — the same shape as the conda failure the ComfyUI
	// claw paid for.
	UVProject = "/root/faster-whisper-server"

	// ASGIFactory is the application the server exposes. A *factory* rather
	// than an instance, which is why the launcher passes --factory: uvicorn
	// given a factory without the flag reports that the target is not an ASGI
	// application, having already loaded the model.
	ASGIFactory = "faster_whisper_server.main:create_app"

	// LaunchTimeout bounds the command that starts the server. Starting a
	// server is a sub-second operation; anything longer is the SSH exec
	// channel being held open, which on ComfyUI cost a thirty-eight minute
	// rental that nothing could reach.
	LaunchTimeout = 60 * time.Second

	launchScriptPath = "/root/.larri-whisper-launch.sh"
)

// Runtime is the faster-whisper-server adapter.
type Runtime struct {
	// ImageRef is the container image. Empty means DefaultImage.
	ImageRef string

	// Model is the repository to transcribe with. Empty means DefaultModel.
	Model string

	// ComputeType is CTranslate2's precision. Empty means DefaultComputeType.
	ComputeType string

	// Language pins the spoken language, skipping detection. Empty means
	// detect.
	Language string

	// ModelBytes is what the model is expected to weigh, for progress. Zero
	// means the fetch reports bytes without a percentage.
	ModelBytes uint64

	// FetchStall ends a download that has stopped growing. Zero means eight
	// minutes.
	FetchStall time.Duration

	// PollInterval is how often the fetch is measured. Zero means fifteen
	// seconds.
	PollInterval time.Duration

	hfToken secret.Secret
	logPath string

	// launch is how this image exposes the server, discovered during
	// bootstrap rather than assumed. See findServer.
	launch serverEntry
}

var (
	_ runtime.Workload          = (*Runtime)(nil)
	_ runtime.CredentialTaker   = (*Runtime)(nil)
	_ runtime.LogWriter         = (*Runtime)(nil)
	_ runtime.LivenessChecker   = (*Runtime)(nil)
	_ runtime.SecurityNoter     = (*Runtime)(nil)
	_ runtime.FailureClassifier = (*Runtime)(nil)
)

// New builds the adapter.
func New() *Runtime {
	return &Runtime{
		ImageRef:     DefaultImage,
		Model:        DefaultModel,
		ComputeType:  DefaultComputeType,
		PollInterval: 15 * time.Second,
		logPath:      LogPathDefault,
	}
}

func (r *Runtime) Kind() core.RuntimeKind { return core.RuntimeWhisper }

// Protocol reports the audio surface, which is not the chat surface.
//
// The distinction is the whole reason the constant exists. This server is
// OpenAI-compatible in every respect a transcription client cares about and
// answers no completion at all, so a caller holding it can find that out here
// rather than from a 404 on a rig that is already billing.
func (r *Runtime) Protocol() runtime.Protocol { return runtime.ProtocolOpenAIAudio }

// Requires reports the hardware floors to apply during selection.
func (r *Runtime) Requires() runtime.Requirements {
	return runtime.Requirements{
		MinComputeCapability: MinComputeCapability,
		MinCUDA:              MinCUDA,
		Vendor:               "nvidia",
		Why:                  "the faster-whisper-server image",
		// A transcription server pins one device. Selection sizes against a
		// single card for the same reason sizing.PlanSpeech does.
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
func (r *Runtime) SecurityNotes() []string {
	return []string{
		"the transcription server has no credential of its own; the loopback bind and the ssh tunnel are what protect it",
		"audio sent for transcription, and the text it produces, are visible to the host, which has root",
	}
}

func (r *Runtime) model() string {
	if r.Model == "" {
		return DefaultModel
	}
	return r.Model
}

func (r *Runtime) computeType() string {
	if r.ComputeType == "" {
		return DefaultComputeType
	}
	return r.ComputeType
}

// serverEntry is how a particular image exposes the server.
//
// Discovered rather than hardcoded, for the reason §4a gives and ComfyUI paid
// for: an assumption about someone else's image that turns out to be wrong is
// discovered on a machine that is already billing. A console script, a module
// name and an import are each cheap to test over one SSH round trip.
type serverEntry struct {
	// Python is an interpreter that can import the server. Usually the one
	// inside the project's virtualenv, which is the whole reason this is
	// discovered: a uv project keeps it in .venv and puts it on PATH through
	// a Dockerfile ENV that a non-interactive SSH session does not inherit.
	Python string

	// Dir is the project to run from, when the image ships one.
	Dir string

	// Script is an executable on PATH, if the image ships one instead.
	Script string
}

// launchCommand renders the command that starts the server in the foreground.
func (e serverEntry) launchCommand() string {
	if e.Script != "" {
		return fmt.Sprintf("%s --factory %s --host %s --port %d",
			shellQuote(e.Script), ASGIFactory, runtime.Loopback, RemotePort)
	}
	python := e.Python
	if python == "" {
		python = "python3"
	}
	// --factory because the target is a factory function. Without it uvicorn
	// reports that the target is not an ASGI application, and it reports it
	// after loading the model rather than before.
	return fmt.Sprintf("%s -m uvicorn --factory %s --host %s --port %d",
		shellQuote(python), ASGIFactory, runtime.Loopback, RemotePort)
}

func (e serverEntry) empty() bool { return e.Python == "" && e.Script == "" }

// findServerCmd asks the image how it exposes the server.
//
// Everything it prints is evidence, including the negative results: a failure
// here has to say what was looked for and what was found, or the operator is
// left with "not found" about a machine they cannot inspect any more.
const findServerCmd = `
for d in ` + UVProject + ` /app /srv/faster-whisper-server /opt/faster-whisper-server; do
  [ -f "$d/pyproject.toml" ] && echo "PROJECT $d"
done
for v in ` + UVProject + `/.venv /app/.venv /opt/venv /usr/local/venv /opt/conda; do
  [ -x "$v/bin/python" ] && "$v/bin/python" -c 'import faster_whisper_server' 2>/dev/null \
    && echo "PYTHON $v/bin/python"
done
for py in python3 python; do
  "$py" -c 'import faster_whisper_server' 2>/dev/null && echo "PYTHON $py"
done
for p in faster-whisper-server speaches; do
  c=$(command -v "$p" 2>/dev/null) && echo "SCRIPT $c"
done
for v in ` + UVProject + `/.venv /app/.venv /opt/venv /opt/conda; do
  [ -x "$v/bin/python" ] && echo "VENV $v/bin/python"
done
for py in python3 python; do
  "$py" -c 'import faster_whisper' 2>/dev/null && echo "ENGINE $py"
done
echo "PATH $PATH"
echo DONE
`

// parseServer reads the probe's output: how to start the server, or the
// evidence for why it cannot be started.
func parseServer(out string) (serverEntry, string) {
	var e serverEntry
	var saw []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "PROJECT":
			if e.Dir == "" {
				e.Dir = f[1]
			}
			saw = append(saw, "project "+f[1])
		case "PYTHON":
			// An interpreter that can import the server, which is the thing
			// actually needed; the first one wins because the probe lists the
			// project's own virtualenv before anything on PATH.
			if e.Python == "" {
				e.Python = f[1]
			}
			saw = append(saw, "server importable by "+f[1])
		case "SCRIPT":
			if e.Script == "" {
				e.Script = f[1]
			}
			saw = append(saw, "script "+f[1])
		case "VENV":
			saw = append(saw, "venv python "+f[1])
		case "ENGINE":
			saw = append(saw, "faster_whisper at "+f[1])
		case "PATH":
			saw = append(saw, "path "+f[1])
		}
	}
	if len(saw) == 0 {
		return e, ""
	}
	return e, ": saw " + strings.Join(saw, ", ")
}

// Bootstrap prepares the host: find the server, prove it imports, fetch the
// model.
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
		Message: "image supplied by the provider; verifying the server"})

	out, err := sess.Run(ctx, findServerCmd)
	if err != nil && len(out) == 0 {
		return errs.Newf(errs.ClassHostFailure, "whisper.Bootstrap",
			"could not inspect the host: %v", err)
	}
	entry, saw := parseServer(string(out))
	if entry.empty() {
		// The image is what the operator asked the provider for, so a server
		// that is not in it is a configuration problem rather than a bad
		// machine: the next host runs the same image and fails identically
		// (FR-PROV-05).
		return errs.Newf(errs.ClassModelFailure, "whisper.Bootstrap",
			"no transcription server in image %s%s", r.Image(spec, plan), saw)
	}
	r.launch = entry
	send(runtime.Progress{Phase: runtime.PhaseImagePull,
		Message: "server found: " + entry.launchCommand()})

	// Before the download, which is the last moment this is cheap to discover
	// (§4a). A CUDA that cannot initialise here costs one SSH round trip; the
	// same fault after the model has been fetched costs three gigabytes and
	// the minutes of billing that carried them.
	if err := r.smokeTest(ctx, sess, send); err != nil {
		return err
	}
	return r.fetchModel(ctx, sess, send)
}

// smokeTest proves the engine loads and sees a GPU before anything large is
// downloaded.
func (r *Runtime) smokeTest(ctx context.Context, sess runtime.Session,
	send func(runtime.Progress)) error {

	send(runtime.Progress{Phase: runtime.PhaseImagePull, Message: "checking cuda"})

	python := r.launch.Python
	if python == "" {
		python = "python3"
	}
	cmd := fmt.Sprintf(
		`%s -c 'import ctranslate2 as c; n=c.get_cuda_device_count(); print("CUDA_DEVICES", n)' 2>&1`,
		shellQuote(python))
	out, err := sess.Run(ctx, cmd)
	text := string(out)
	if err != nil && text == "" {
		return errs.Newf(errs.ClassHostFailure, "whisper.Bootstrap",
			"could not run the cuda check: %v", err)
	}
	if !strings.Contains(text, "CUDA_DEVICES") {
		// An import that fails is the image's problem and travels with it.
		return errs.Newf(errs.ClassModelFailure, "whisper.Bootstrap",
			"the image cannot load ctranslate2: %s", lastLine(text))
	}
	if strings.Contains(text, "CUDA_DEVICES 0") {
		// No device is the *host's* problem: the next machine may have one.
		// Worth failing on rather than proceeding, because the server starts
		// happily on the CPU and transcribes at a fraction of realtime, which
		// is a rig paying GPU rates for nothing and looks healthy from
		// outside.
		return errs.Newf(errs.ClassHostFailure, "whisper.Bootstrap",
			"ctranslate2 sees no cuda device on this host")
	}
	send(runtime.Progress{Phase: runtime.PhaseImagePull, Percent: 100,
		Message: "cuda ok"})
	return nil
}

// ClassifyFailure reads the server's own log and says whose fault a failure
// is, so a configuration fault is not retried on three more machines.
func (r *Runtime) ClassifyFailure(log string) errs.Class {
	l := strings.ToLower(log)
	switch {
	case strings.Contains(l, "out of memory"),
		strings.Contains(l, "cuda error"):
		return errs.ClassModelFailure
	case strings.Contains(l, "modulenotfounderror"),
		strings.Contains(l, "importerror"),
		strings.Contains(l, "no such file or directory: 'uvicorn'"):
		return errs.ClassModelFailure
	case strings.Contains(l, "repository not found"),
		strings.Contains(l, "401 client error"),
		strings.Contains(l, "gated"):
		return errs.ClassModelFailure
	}
	return errs.ClassUnknown
}

// stopServersCmd clears a previous server before a new one starts.
//
// Its own command, never combined with the launch, so the pattern cannot match
// the shell issuing it — a mistake the vLLM path made three separate times.
// It also clears whatever the image's own entrypoint may have started, which
// is why the launch does not depend on knowing whether it started anything.
const stopServersCmd = `pkill -f 'uvicorn' 2>/dev/null; pkill -f '[f]aster-whisper-server' 2>/dev/null; true`

// Launch starts the server bound to loopback.
func (r *Runtime) Launch(ctx context.Context, sess runtime.Session,
	spec core.ModelSpec, plan core.SizingPlan) (runtime.Endpoint, error) {

	if r.launch.empty() {
		return runtime.Endpoint{}, errs.Newf(errs.ClassModelFailure, "whisper.Launch",
			"bootstrap did not find a server to start")
	}
	ep := runtime.Endpoint{
		Host:  runtime.Loopback,
		Port:  RemotePort,
		Model: spec.ServedName,
		// No Key: faster-whisper-server has no server-side credential to hold
		// one. SecurityNotes says so rather than leaving it to be inferred.
	}
	// FR-SEC-08: the bind address is computed here and is not configurable.
	if !ep.Valid() {
		return runtime.Endpoint{}, errs.Newf(errs.ClassModelFailure, "whisper.Launch",
			"invalid bind address %s: loopback only", ep.Host)
	}

	lctx, cancel := context.WithTimeout(ctx, LaunchTimeout)
	defer cancel()

	_, _ = sess.Run(lctx, stopServersCmd)

	if err := r.startServer(lctx, sess); err != nil {
		return runtime.Endpoint{}, err
	}
	return ep, nil
}

// launchScript is what actually starts the server.
//
// A file rather than a one-liner, and no `cd X && ...` prefix, because the
// shape is what matters. ComfyUI hung twice at this exact point: sshd holds an
// exec channel open while any live process has a descriptor on it, and a
// compound command makes the whole thing the asynchronous list so the forked
// subshell inherits the channel. This is the shape already proven on this
// hardware, copied rather than re-derived.
//
// The settings go in the environment because that is how this image is
// configured. If a name here is wrong the server starts with its own default
// model, which readiness catches by asking /v1/models what it actually loaded
// — the cheap failure rather than a silent one.
func (r *Runtime) launchScript() string {
	var b strings.Builder
	fmt.Fprintf(&b, "export WHISPER__MODEL=%s\n", shellQuote(r.model()))
	fmt.Fprintf(&b, "export WHISPER__INFERENCE_DEVICE=cuda\n")
	fmt.Fprintf(&b, "export WHISPER__COMPUTE_TYPE=%s\n", shellQuote(r.computeType()))
	fmt.Fprintf(&b, "export UVICORN_HOST=%s\n", runtime.Loopback)
	fmt.Fprintf(&b, "export UVICORN_PORT=%d\n", RemotePort)
	fmt.Fprintf(&b, "export HF_HOME=%s\n", shellQuote(CacheRoot))
	if !r.hfToken.Empty() {
		fmt.Fprintf(&b, "export HF_TOKEN=%s\n", shellQuote(r.hfToken.Reveal()))
	}
	// The cd belongs here, on its own line inside the script, and never in the
	// launcher. A `cd X && ...` makes the whole compound the asynchronous
	// list, so the forked subshell inherits the SSH channel's descriptors and
	// the call never returns — which is what hung the ComfyUI claw twice.
	if r.launch.Dir != "" {
		fmt.Fprintf(&b, "cd %s\n", shellQuote(r.launch.Dir))
	}
	fmt.Fprintf(&b, "exec %s\n", r.launch.launchCommand())
	return b.String()
}

// startServer writes the start script and launches it detached.
func (r *Runtime) startServer(ctx context.Context, sess runtime.Session) error {
	write := fmt.Sprintf("cat > %s <<'LARRI_LAUNCH_EOF'\n%s\nLARRI_LAUNCH_EOF\nchmod 700 %s",
		shellQuote(launchScriptPath), r.launchScript(), shellQuote(launchScriptPath))
	if _, err := sess.Run(ctx, write); err != nil {
		return errs.Newf(errs.ClassHostFailure, "whisper.Launch",
			"write the start script: %v", err)
	}

	launch := fmt.Sprintf("setsid nohup sh %s </dev/null >%s 2>&1 & echo LAUNCHED",
		shellQuote(launchScriptPath), shellQuote(r.LogPath()))
	out, err := sess.Run(ctx, launch)
	if err != nil {
		return errs.Newf(errs.ClassHostFailure, "whisper.Launch",
			"start server: %v", err)
	}
	// The marker proves the shell got far enough to fork and return. Without
	// it a command that produced nothing looks exactly like one that worked,
	// which is how a hang was once mistaken for a launch.
	if !strings.Contains(string(out), "LAUNCHED") {
		return errs.Newf(errs.ClassHostFailure, "whisper.Launch",
			"start server: no confirmation from the host: %s", lastLine(string(out)))
	}
	return nil
}

// Ready performs a real round-trip: a transcription, through the tunnel.
//
// Three things in order, cheapest first. The server answers; it loaded the
// model that was asked for rather than its own default, which is what catches
// a setting this adapter got wrong; and it transcribes an audio file end to
// end, which is the claim READY is supposed to make.
//
// The audio is generated rather than shipped, so there is no clip in the
// repository and no licence attached to one. It is noise rather than silence
// deliberately: the server's voice-activity filter can skip a silent file
// without running the model at all, which would make this prove the HTTP path
// and nothing else.
func (r *Runtime) Ready(ctx context.Context, ep runtime.Endpoint, spec core.ModelSpec) error {
	c := &Client{
		Addr:  LocalAddr(ep.Host, ep.Port),
		Token: ep.Key.Reveal(),
		Probe: true,
	}
	loaded, err := c.Models(ctx)
	if err != nil {
		return err
	}
	if !namesModel(loaded, r.model()) {
		return errs.Newf(errs.ClassModelFailure, "whisper.Ready",
			"the server loaded %s, not %s", strings.Join(loaded, ", "), r.model())
	}
	text, err := c.Transcribe(ctx, ReadyClip(), "ready.wav")
	if err != nil {
		return err
	}
	_ = text // Whatever it heard in noise; that it answered at all is the claim.
	return nil
}

// namesModel reports whether the server's model list includes the one asked
// for. Servers report either the full repository or its bare name.
func namesModel(loaded []string, want string) bool {
	short := want
	if i := strings.LastIndex(want, "/"); i >= 0 {
		short = want[i+1:]
	}
	for _, m := range loaded {
		if m == want || m == short {
			return true
		}
	}
	return false
}

// Alive reports whether the server process is still on the host.
func (r *Runtime) Alive(ctx context.Context, sess runtime.Session) (bool, error) {
	out, err := sess.Run(ctx, `pgrep -f 'uvicorn|[f]aster-whisper-server' >/dev/null && echo ALIVE || echo GONE`)
	if err != nil && len(out) == 0 {
		return false, err
	}
	return strings.Contains(string(out), "ALIVE"), nil
}

// Logs streams the server's output for diagnosis.
func (r *Runtime) Logs(ctx context.Context, sess runtime.Session, tail int) (io.ReadCloser, error) {
	if tail <= 0 {
		tail = 200
	}
	out, err := sess.Run(ctx, fmt.Sprintf("tail -n %d %s 2>/dev/null", tail, shellQuote(r.LogPath())))
	if err != nil && len(out) == 0 {
		return nil, errs.Newf(errs.ClassHostFailure, "whisper.Logs", "read the log: %v", err)
	}
	return io.NopCloser(strings.NewReader(string(out))), nil
}

// Stop halts the server.
func (r *Runtime) Stop(ctx context.Context, sess runtime.Session) error {
	_, _ = sess.Run(ctx, stopServersCmd)
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			if len(t) > 240 {
				t = t[:240]
			}
			return t
		}
	}
	return ""
}
