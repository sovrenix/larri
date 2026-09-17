// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package whisper

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
)

// hostSim answers the shell commands the adapter issues, so the whole
// bootstrap runs offline. It records what it was asked, because several of the
// assertions here are about the *shape* of a command rather than its result —
// two live hangs on the ComfyUI claw were caused by shape alone.
type hostSim struct {
	mu sync.Mutex

	ran []string

	// noServer makes the image look like one that does not carry the server.
	noServer bool
	// cudaDevices is what ctranslate2 reports.
	cudaDevices int
	// importFails makes the ctranslate2 import raise.
	importFails bool
	// fetchAfter is how many done-checks pass before the marker appears.
	fetchAfter int

	checks int
	bytes  uint64
}

func newHost() *hostSim { return &hostSim{cudaDevices: 1} }

func (h *hostSim) Run(_ context.Context, cmd string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ran = append(h.ran, cmd)

	switch {
	case strings.Contains(cmd, "command -v"):
		if h.noServer {
			return []byte("PATH /usr/bin\nDONE\n"), nil
		}
		// What the real image answers: a uv project whose interpreter lives in
		// .venv and is invisible to the stock PATH a non-interactive SSH
		// session gets. A first paid run failed on exactly this.
		return []byte("PROJECT /root/faster-whisper-server\n" +
			"PYTHON /root/faster-whisper-server/.venv/bin/python\n" +
			"VENV /root/faster-whisper-server/.venv/bin/python\n" +
			"PATH /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n" +
			"DONE\n"), nil

	case strings.Contains(cmd, "ctranslate2"):
		if h.importFails {
			return []byte("ModuleNotFoundError: No module named 'ctranslate2'\n"), nil
		}
		return []byte("CUDA_DEVICES " + itoa(h.cudaDevices) + "\n"), nil

	case strings.Contains(cmd, "rm -f"):
		return nil, nil

	case strings.HasPrefix(cmd, "cat > "):
		return nil, nil

	case strings.Contains(cmd, "STARTED"):
		return []byte("STARTED\n"), nil

	case strings.Contains(cmd, fetchDoneMarker) && strings.Contains(cmd, "test -f"):
		h.checks++
		h.bytes += 1 << 30
		if h.checks > h.fetchAfter {
			return []byte("DONE\n"), nil
		}
		return []byte("WORKING\n"), nil

	case strings.Contains(cmd, "du -sb"):
		return []byte(utoa(h.bytes) + "\n"), nil

	case strings.Contains(cmd, "LAUNCHED"):
		return []byte("LAUNCHED\n"), nil

	case strings.HasPrefix(cmd, "pkill"):
		return nil, nil

	case strings.Contains(cmd, "pgrep"):
		return []byte("ALIVE\n"), nil

	case strings.HasPrefix(cmd, "tail"):
		return []byte("nothing to report\n"), nil
	}
	return nil, nil
}

func (h *hostSim) Dial(context.Context, int) (io.ReadWriteCloser, error) { return nil, nil }
func (h *hostSim) Close() error                                          { return nil }

func (h *hostSim) commands() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.ran...)
}

var _ runtime.Session = (*hostSim)(nil)

func itoa(n int) string { return string(rune('0' + n)) }
func utoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func newFast() *Runtime {
	r := New()
	r.PollInterval = time.Millisecond
	r.ModelBytes = 3 << 30
	return r
}

// The whole path, offline: find the server, prove cuda, fetch the model,
// launch it, and transcribe through the endpoint.
func TestOneTranscriptionAllTheWayThrough(t *testing.T) {
	host := newHost()
	host.fetchAfter = 2
	r := newFast()

	progress := make(chan runtime.Progress, 64)
	spec := core.ModelSpec{ServedName: "whisper"}
	if err := r.Bootstrap(context.Background(), host, spec, core.SizingPlan{}, progress); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	close(progress)

	var sawDownload bool
	for p := range progress {
		if p.Phase == runtime.PhaseWeightsDownload {
			sawDownload = true
		}
	}
	if !sawDownload {
		t.Error("the fetch reported no progress, so a stalled download would be invisible")
	}

	ep, err := r.Launch(context.Background(), host, spec, core.SizingPlan{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if ep.Host != runtime.Loopback {
		t.Errorf("bound %s, not loopback: a routable inference port is unauthenticated "+
			"access to hardware the operator is paying for", ep.Host)
	}
	if ep.Port != RemotePort {
		t.Errorf("port = %d, want %d", ep.Port, RemotePort)
	}

	// The server, answering at the local end of what would be the tunnel.
	srv := fakeServer(t, DefaultModel, "the quick brown fox")
	defer srv.Close()
	r2 := newFast()
	r2.launch = r.launch
	if err := r2.Ready(context.Background(), runtime.Endpoint{
		Host: hostOf(srv.URL), Port: portOf(srv.URL), Model: "whisper",
	}, spec); err != nil {
		t.Fatalf("ready: %v", err)
	}
}

// Two live hangs on the ComfyUI claw came from the launcher's shape alone, so
// the shape is asserted rather than the outcome. A compound command makes the
// whole thing the asynchronous list, and the forked subshell inherits the SSH
// channel's descriptors.
func TestTheLauncherCannotHoldTheSSHChannelOpen(t *testing.T) {
	host := newHost()
	host.fetchAfter = 0
	r := newFast()
	spec := core.ModelSpec{ServedName: "whisper"}
	if err := r.Bootstrap(context.Background(), host, spec, core.SizingPlan{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Launch(context.Background(), host, spec, core.SizingPlan{}); err != nil {
		t.Fatal(err)
	}
	var launcher string
	for _, c := range host.commands() {
		if strings.Contains(c, "setsid nohup") && strings.Contains(c, launchScriptPath) {
			launcher = c
		}
	}
	if launcher == "" {
		t.Fatal("no detached launch was issued")
	}
	if strings.Contains(launcher, "&&") {
		t.Errorf("the launcher is a compound command, which is what hung twice: %s", launcher)
	}
	if !strings.Contains(launcher, "</dev/null") {
		t.Errorf("stdin is still on the ssh channel: %s", launcher)
	}
}

// The image is what the operator asked the provider for, so a server missing
// from it travels to the next machine unchanged. Retrying that is what a
// misclassification costs — on ComfyUI it was three rentals.
func TestAnImageWithNoServerIsNotTheHostsFault(t *testing.T) {
	host := newHost()
	host.noServer = true
	err := New().Bootstrap(context.Background(), host,
		core.ModelSpec{ServedName: "whisper"}, core.SizingPlan{}, nil)
	if err == nil {
		t.Fatal("an image with no server bootstrapped successfully")
	}
	if errs.ClassOf(err) != errs.ClassModelFailure {
		t.Errorf("class = %v, want model-failure: the next host runs the same image",
			errs.ClassOf(err))
	}
	// The evidence has to travel with the error; the machine is gone by the
	// time anyone reads it.
	if !strings.Contains(err.Error(), "saw") {
		t.Errorf("the error carries no evidence of what was looked for: %v", err)
	}
}

// A host with no CUDA is the host's problem: the next machine may have one.
// Failing here rather than proceeding matters because the server starts
// happily on the CPU and looks entirely healthy from outside.
func TestAHostWithNoCUDAIsWorthReplacing(t *testing.T) {
	host := newHost()
	host.cudaDevices = 0
	err := New().Bootstrap(context.Background(), host,
		core.ModelSpec{ServedName: "whisper"}, core.SizingPlan{}, nil)
	if err == nil {
		t.Fatal("a host with no cuda device bootstrapped successfully")
	}
	if errs.ClassOf(err) != errs.ClassHostFailure {
		t.Errorf("class = %v, want host-failure", errs.ClassOf(err))
	}
}

// The cuda check runs before the model is fetched, which is the last moment it
// is cheap. The same fault after the download costs three gigabytes and the
// billing that carried them.
func TestTheCUDACheckRunsBeforeTheDownload(t *testing.T) {
	host := newHost()
	host.importFails = true
	r := newFast()
	err := r.Bootstrap(context.Background(), host,
		core.ModelSpec{ServedName: "whisper"}, core.SizingPlan{}, nil)
	if err == nil {
		t.Fatal("an image that cannot import ctranslate2 bootstrapped successfully")
	}
	for _, c := range host.commands() {
		if strings.Contains(c, fetchScriptPath) {
			t.Fatal("the model fetch was started despite the cuda check failing")
		}
	}
}

// Readiness asks what the server actually loaded, because the settings that
// choose it are environment variables in someone else's image. A name this
// adapter got wrong does not fail — the server starts with its own default.
func TestReadinessRefusesAServerServingADifferentModel(t *testing.T) {
	srv := fakeServer(t, "Systran/faster-whisper-tiny", "hello")
	defer srv.Close()

	r := New()
	r.launch = serverEntry{Python: "/root/faster-whisper-server/.venv/bin/python"}
	err := r.Ready(context.Background(), runtime.Endpoint{
		Host: hostOf(srv.URL), Port: portOf(srv.URL),
	}, core.ModelSpec{ServedName: "whisper"})
	if err == nil {
		t.Fatal("a server holding the wrong model was reported ready")
	}
	if !strings.Contains(err.Error(), "large-v3") {
		t.Errorf("the error does not name what was asked for: %v", err)
	}
}

// Noise rather than silence, because a voice-activity filter can skip a silent
// file without running the model at all — which would make readiness prove the
// HTTP path and nothing else.
func TestTheReadyClipIsAudibleAndDeterministic(t *testing.T) {
	a, b := ReadyClip(), ReadyClip()
	if string(a) != string(b) {
		t.Error("two clips differ, so a difference in the response is not a difference in the rig")
	}
	if len(a) < 44 || string(a[:4]) != "RIFF" || string(a[8:12]) != "WAVE" {
		t.Fatal("the clip is not a wav file")
	}
	var nonZero int
	for _, x := range a[44:] {
		if x != 0 {
			nonZero++
		}
	}
	if nonZero < len(a[44:])/4 {
		t.Errorf("only %d of %d sample bytes are non-zero; a vad filter would skip this",
			nonZero, len(a[44:]))
	}
}

// fakeServer is the transcription server at the local end of the tunnel.
func fakeServer(t *testing.T, model, text string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": model}},
		})
	})
	mux.HandleFunc("/v1/audio/transcriptions", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "not multipart", http.StatusBadRequest)
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "no file part", http.StatusBadRequest)
			return
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		if len(b) < 44 || string(b[:4]) != "RIFF" {
			http.Error(w, "not a wav", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"text": text})
	})
	return httptest.NewServer(mux)
}

func hostOf(u string) string {
	s := strings.TrimPrefix(u, "http://")
	if i := strings.LastIndex(s, ":"); i >= 0 {
		return s[:i]
	}
	return s
}

func portOf(u string) int {
	s := strings.TrimPrefix(u, "http://")
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return 0
	}
	n := 0
	for _, c := range s[i+1:] {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// The image's own Cmd is ["uv","run","uvicorn","--factory",
// "faster_whisper_server.main:create_app"], read from the registry rather than
// guessed. The target is a factory function, and uvicorn handed one without
// --factory reports that it is not an ASGI application — after loading the
// model rather than before.
func TestTheLauncherPassesTheFactoryFlag(t *testing.T) {
	host := newHost()
	r := newFast()
	spec := core.ModelSpec{ServedName: "whisper"}
	if err := r.Bootstrap(context.Background(), host, spec, core.SizingPlan{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Launch(context.Background(), host, spec, core.SizingPlan{}); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, c := range host.commands() {
		if strings.HasPrefix(c, "cat > ") && strings.Contains(c, launchScriptPath) {
			script = c
		}
	}
	if script == "" {
		t.Fatal("no launch script was written")
	}
	if !strings.Contains(script, "--factory "+ASGIFactory) {
		t.Errorf("the launcher does not pass the factory:\n%s", script)
	}
	// The project directory, because the server is a uv project and its
	// interpreter is not the one on PATH.
	if !strings.Contains(script, "cd '"+UVProject+"'") {
		t.Errorf("the script does not enter the project directory:\n%s", script)
	}
	if !strings.Contains(script, UVProject+"/.venv/bin/python") {
		t.Errorf("the script does not use the project's own interpreter:\n%s", script)
	}
	// Loopback, and not the 0.0.0.0 the image defaults to (FR-SEC-08).
	if !strings.Contains(script, "--host 127.0.0.1") {
		t.Errorf("the server was not bound to loopback:\n%s", script)
	}
}
