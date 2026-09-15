// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package comfy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/workflow"
)

// fakeSession records what was run and answers from a scripted table.
type fakeSession struct {
	mu   sync.Mutex
	ran  []string
	rule func(cmd string) (string, error)
}

func (f *fakeSession) Run(_ context.Context, cmd string) ([]byte, error) {
	f.mu.Lock()
	f.ran = append(f.ran, cmd)
	f.mu.Unlock()
	if f.rule == nil {
		return nil, nil
	}
	out, err := f.rule(cmd)
	return []byte(out), err
}

func (f *fakeSession) Dial(context.Context, int) (io.ReadWriteCloser, error) { return nil, nil }
func (f *fakeSession) Close() error                                          { return nil }

func (f *fakeSession) all() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.ran, "\n")
}

var _ runtime.Session = (*fakeSession)(nil)

// ---- the widened abstraction ------------------------------------------

// ComfyUI is a Workload and must not be usable as a Runtime. The compiler is
// the enforcement; this records why, so a future edit that "simplifies" the
// two interfaces back together fails here with a reason attached.
func TestComfyIsAWorkloadAndNotARuntime(t *testing.T) {
	var w runtime.Workload = New(nil, nil, nil)
	if w.Protocol() == runtime.ProtocolOpenAI {
		t.Fatal("comfyui claims to serve /v1; nothing above the workload layer " +
			"may be allowed to issue a completion against it")
	}
	if !w.Protocol().Browser() {
		t.Error("comfyui is a browser surface and must say so: the local " +
			"listener needs a cookie credential and a browser cannot send a bearer")
	}
	if w.Kind() != core.RuntimeComfyUI {
		t.Errorf("kind = %q", w.Kind())
	}
}

// ---- the fetch script --------------------------------------------------

// A workflow is an artefact operators download from strangers and open without
// reading, and the fetch runs as root. A checkpoint named "../../.ssh/…" must
// never be written where it asks to be.
func TestSafeNameRefusesEscapes(t *testing.T) {
	for _, bad := range []string{
		"../../../root/.ssh/authorized_keys",
		"/etc/passwd",
		"sub/../../escape.safetensors",
		"",
	} {
		if err := SafeName(bad); err == nil {
			t.Errorf("accepted %q as a model name", bad)
		}
	}
	for _, ok := range []string{
		"sd_xl_base_1.0.safetensors",
		"sdxl/refiner.safetensors",
		"a.b.c/model-v2_final.safetensors",
	} {
		if err := SafeName(ok); err != nil {
			t.Errorf("refused a legitimate name %q: %v", ok, err)
		}
	}
}

// The same names reach a shell. Quoting is what stands between a graph and
// arbitrary root execution on a machine holding the operator's HF token.
func TestScriptQuotesHostileNames(t *testing.T) {
	d := Download{Dir: ModelsDir, Items: []Item{{
		URL:   "https://example.invalid/x",
		Kind:  workflow.KindCheckpoint,
		Name:  "evil'; touch /tmp/pwned; echo '.safetensors",
		Bytes: 10,
	}}}
	script, err := d.Script(false)
	if err != nil {
		t.Fatalf("a quotable name was refused: %v", err)
	}
	// The dangerous text must appear only inside a quoted word, never as a
	// command the shell would run.
	if strings.Contains(script, "; touch /tmp/pwned; echo ") &&
		!strings.Contains(script, `'\''; touch /tmp/pwned; echo '\''`) {
		t.Fatalf("the name escaped its quoting:\n%s", script)
	}
}

func TestScriptRefusesAnEscapingName(t *testing.T) {
	d := Download{Dir: ModelsDir, Items: []Item{{
		URL: "https://example.invalid/x", Kind: workflow.KindCheckpoint,
		Name: "../../etc/cron.d/pwn", Bytes: 10,
	}}}
	if _, err := d.Script(false); err == nil {
		t.Fatal("built a script that writes outside the models directory")
	}
}

// Every file lands under models/<kind>/, because a checkpoint written into
// models/loras is one ComfyUI never finds.
func TestScriptPlacesFilesByKind(t *testing.T) {
	d := Download{Dir: ModelsDir, Items: []Item{
		{URL: "u1", Kind: workflow.KindCheckpoint, Name: "a.safetensors", Bytes: 1},
		{URL: "u2", Kind: workflow.KindLoRA, Name: "b.safetensors", Bytes: 2},
	}}
	script, err := d.Script(false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		ModelsDir + "/checkpoints/a.safetensors",
		ModelsDir + "/loras/b.safetensors",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script does not write %s", want)
		}
	}
}

// A file already present at the right size is not fetched again. A rig
// replaced under a live session must not pay for the same 7 GB twice.
func TestScriptSkipsWhatIsAlreadyThere(t *testing.T) {
	d := Download{Dir: ModelsDir, Items: []Item{
		{URL: "u", Kind: workflow.KindCheckpoint, Name: "a.safetensors", Bytes: 4242},
	}}
	script, _ := d.Script(false)
	if !strings.Contains(script, "4242") {
		t.Error("the expected size is not used to decide whether to skip")
	}
	if !strings.Contains(script, "stat -c") {
		t.Error("nothing measures the existing file")
	}
}

// A truncated checkpoint fails at load with a stack trace that says nothing
// about the network, so the size is verified before the file gets its name.
func TestScriptVerifiesBeforeRenaming(t *testing.T) {
	d := Download{Dir: ModelsDir, Items: []Item{
		{URL: "u", Kind: workflow.KindCheckpoint, Name: "a.safetensors", Bytes: 900},
	}}
	script, _ := d.Script(false)
	short := strings.Index(script, "SHORT")
	mv := strings.Index(script, "mv ")
	if short < 0 || mv < 0 || short > mv {
		t.Fatalf("the size check does not precede the rename:\n%s", script)
	}
	if !strings.Contains(script, ".part") {
		t.Error("the download does not use a temporary name")
	}
}

// Invariant 9: a credential is never echoed. The token must reach the host in
// a file, and never in a command LARRI builds and might log.
func TestTheHuggingFaceTokenNeverEntersACommandLine(t *testing.T) {
	const tok = "hf_SECRETVALUE123"
	f := &fakeSession{}
	if err := WriteCredential(context.Background(), f, secret.New(tok)); err != nil {
		t.Fatal(err)
	}
	d := Download{Dir: ModelsDir, Items: []Item{
		{URL: "u", Kind: workflow.KindCheckpoint, Name: "a.safetensors", Bytes: 1},
	}}
	script, err := d.Script(true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, tok) {
		t.Error("the token is embedded in the fetch script")
	}
	if !strings.Contains(script, "-K ") {
		t.Error("curl is not reading its credential from a config file")
	}
	// The one command that must carry it is the write itself, and nothing else.
	writes := 0
	for _, cmd := range f.ran {
		if strings.Contains(cmd, tok) {
			writes++
		}
	}
	if writes != 1 {
		t.Errorf("the token appeared in %d commands, want exactly the credential write", writes)
	}
}

func TestNoTokenClearsTheCredentialFile(t *testing.T) {
	f := &fakeSession{}
	if err := WriteCredential(context.Background(), f, secret.Secret{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.all(), "rm -f") {
		t.Error("a stale credential from an earlier rig was left on the host")
	}
}

// The marker file is the completion signal, not the log's last line: a log can
// end mid-word and "ALLDONE" is a string an error could contain.
func TestDoneReadsTheMarkerAndReportsFailures(t *testing.T) {
	ctx := context.Background()
	d := Download{Dir: ModelsDir, Log: FetchLog}

	yes := &fakeSession{rule: func(cmd string) (string, error) {
		if strings.Contains(cmd, "test -f") {
			return "YES", nil
		}
		return "", nil
	}}
	if ok, err := d.Done(ctx, yes); !ok || err != nil {
		t.Errorf("marker present: ok=%v err=%v", ok, err)
	}

	failed := &fakeSession{rule: func(cmd string) (string, error) {
		if strings.Contains(cmd, "test -f") {
			return "NO", nil
		}
		return "fetch a.safetensors\nFAILED a.safetensors\n", nil
	}}
	if _, err := d.Done(ctx, failed); err == nil {
		t.Error("a failed fetch was reported as merely unfinished")
	}

	short := &fakeSession{rule: func(cmd string) (string, error) {
		if strings.Contains(cmd, "test -f") {
			return "NO", nil
		}
		return "SHORT a.safetensors 10 != 900\n", nil
	}}
	if _, err := d.Done(ctx, short); err == nil {
		t.Error("a truncated file was reported as merely unfinished")
	}
}

// ---- readiness ---------------------------------------------------------

// comfyStub is a ComfyUI that renders.
type comfyStub struct {
	*httptest.Server
	cuda      bool
	submitted int
	outputs   []OutputFile
	failWith  string
	mu        sync.Mutex
}

func newStub(cuda bool, outputs []OutputFile) *comfyStub {
	s := &comfyStub{cuda: cuda, outputs: outputs}
	mux := http.NewServeMux()
	mux.HandleFunc("/system_stats", func(w http.ResponseWriter, _ *http.Request) {
		devs := []map[string]any{}
		if s.cuda {
			devs = append(devs, map[string]any{
				"name": "cuda:0 NVIDIA RTX 4090", "type": "cuda",
				"vram_total": 25757220864, "vram_free": 24000000000,
			})
		} else {
			devs = append(devs, map[string]any{"name": "cpu", "type": "cpu"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"devices": devs})
	})
	mux.HandleFunc("/prompt", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.submitted++
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"prompt_id": "p1", "number": 1})
	})
	mux.HandleFunc("/history/", func(w http.ResponseWriter, _ *http.Request) {
		status := map[string]any{"status_str": "success", "completed": true}
		if s.failWith != "" {
			status = map[string]any{"status_str": s.failWith, "completed": true}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"p1": map[string]any{
				"outputs": map[string]any{"9": map[string]any{"images": s.outputs}},
				"status":  status,
			},
		})
	})
	mux.HandleFunc("/queue", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"queue_running": []any{}, "queue_pending": []any{},
		})
	})
	s.Server = httptest.NewServer(mux)
	return s
}

// submissions reads the counter under the lock the handler writes it with.
// Read without it, this is a data race the -race build in CI would fail on,
// and the assertion it supports is the one that proves readiness is a real
// round-trip rather than a health check.
func (s *comfyStub) submissions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.submitted
}

// NFR-05: ready means a real round-trip. For ComfyUI that is a rendered image,
// and it is the operator's own graph that renders it — so READY means "the
// workflow you asked for produced a file".
func TestReadyRendersTheOperatorsGraph(t *testing.T) {
	stub := newStub(true, []OutputFile{{Filename: "out_001.png", Type: "output"}})
	defer stub.Close()

	g, _ := workflow.Parse([]byte(
		`{"1": {"class_type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "a.safetensors"}}}`))
	r := New(g, nil, nil)
	ep := runtime.Endpoint{Host: "127.0.0.1", Port: stubPort(t, stub)}
	if err := r.Ready(context.Background(), ep, core.ModelSpec{}); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if stub.submissions() == 0 {
		t.Error("readiness never submitted the graph; a /system_stats 200 is not a round-trip")
	}
}

// A server that starts on the CPU looks entirely healthy and is the most
// expensive way to render an image there is.
func TestReadyRefusesACPUOnlyServer(t *testing.T) {
	stub := newStub(false, nil)
	defer stub.Close()
	r := New(nil, nil, nil)
	ep := runtime.Endpoint{Host: "127.0.0.1", Port: stubPort(t, stub)}
	err := r.Ready(context.Background(), ep, core.ModelSpec{})
	if err == nil || !strings.Contains(err.Error(), "cuda") {
		t.Fatalf("a cpu-only comfyui was accepted as ready: %v", err)
	}
}

// A graph that completes and writes nothing has not proved anything.
func TestReadyRejectsACompletedGraphWithNoOutput(t *testing.T) {
	stub := newStub(true, nil)
	defer stub.Close()
	g, _ := workflow.Parse([]byte(
		`{"1": {"class_type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "a.safetensors"}}}`))
	r := New(g, nil, nil)
	ep := runtime.Endpoint{Host: "127.0.0.1", Port: stubPort(t, stub)}
	if err := r.Ready(context.Background(), ep, core.ModelSpec{}); err == nil {
		t.Fatal("a graph that produced no file was accepted as ready")
	}
}

// A UI-format graph cannot be submitted, so readiness is weaker — and must say
// so rather than letting READY quietly mean two different things.
func TestUIGraphReadinessIsCaveated(t *testing.T) {
	stub := newStub(true, nil)
	defer stub.Close()
	g, _ := workflow.Parse([]byte(
		`{"nodes": [{"id": 1, "type": "CheckpointLoaderSimple",
		             "widgets_values": ["a.safetensors"]}]}`))
	r := New(g, nil, nil)
	ep := runtime.Endpoint{Host: "127.0.0.1", Port: stubPort(t, stub)}
	if err := r.Ready(context.Background(), ep, core.ModelSpec{}); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if stub.submissions() != 0 {
		t.Error("a ui graph was submitted to /prompt, which rejects it")
	}
	if len(r.Caveats()) == 0 {
		t.Error("the weaker readiness was not disclosed")
	}
}

// FR-SEC-08: the bind address is computed, not configured.
func TestLaunchBindsLoopbackOnly(t *testing.T) {
	f := &fakeSession{rule: func(cmd string) (string, error) {
		if strings.Contains(cmd, "setsid") {
			return "LAUNCHED", nil
		}
		return "", nil
	}}
	r := New(nil, nil, nil)
	r.python = "python3"
	ep, err := r.Launch(context.Background(), f,
		core.ModelSpec{ServedName: "comfyui"}, core.SizingPlan{})
	if err != nil {
		t.Fatal(err)
	}
	if ep.Host != runtime.Loopback {
		t.Errorf("bound %s, want loopback", ep.Host)
	}
	all := f.all()
	if !strings.Contains(all, "--listen 127.0.0.1") {
		t.Error("comfyui was not told to bind loopback; --listen defaults to 0.0.0.0")
	}
	if strings.Contains(all, "--listen 0.0.0.0") {
		t.Error("comfyui was published on every interface")
	}
}

// ComfyUI has no server-side credential. Ollama has the same gap and says so;
// silence here would let an operator assume the stronger guarantee.
func TestSecurityNotesDiscloseTheMissingCredential(t *testing.T) {
	notes := New(nil, nil, nil).SecurityNotes()
	if len(notes) == 0 {
		t.Fatal("no security notes at all")
	}
	if !strings.Contains(strings.Join(notes, " "), "credential") {
		t.Errorf("the missing server-side credential is not disclosed: %v", notes)
	}
	r := New(nil, nil, nil)
	r.AllowPickle = true
	if !strings.Contains(strings.Join(r.SecurityNotes(), " "), "pickle") {
		t.Error("allowing pickle containers was not disclosed at bring-up")
	}
}

// ---- idle accounting ---------------------------------------------------

// Idle reclamation destroys, so what counts as activity decides when a GPU
// stops being paid for. A browser tab polling its queue is not the operator
// working.
func TestOnlyRealWorkResetsTheIdleClock(t *testing.T) {
	work := []struct{ method, path string }{
		{"POST", "/prompt"}, {"POST", "/interrupt"},
		{"POST", "/upload/image"}, {"POST", "/queue"},
	}
	for _, w := range work {
		r := httptest.NewRequest(w.method, w.path, nil)
		if !CountsAsWork(r) {
			t.Errorf("%s %s does not count as work", w.method, w.path)
		}
	}
	chatter := []struct{ method, path string }{
		{"GET", "/queue"}, {"GET", "/history/p1"}, {"GET", "/ws"},
		{"GET", "/view"}, {"GET", "/object_info"}, {"GET", "/"},
		{"GET", "/assets/index.js"},
	}
	for _, c := range chatter {
		r := httptest.NewRequest(c.method, c.path, nil)
		if CountsAsWork(r) {
			t.Errorf("%s %s counted as work; an open tab would hold the rig overnight",
				c.method, c.path)
		}
	}
}

// A render is one short POST followed by minutes of work with no requests at
// all. Without the queue hold, a graph slower than the idle timeout is
// destroyed halfway through.
func TestABusyQueueHoldsTheRigOpen(t *testing.T) {
	// Guarded, because the handler runs on the server's goroutine while the
	// test mutates this from its own. The race detector in CI catches an
	// unguarded version, and a flaky queue-state test would be worse than no
	// test at all.
	var busy atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/queue", func(w http.ResponseWriter, _ *http.Request) {
		running := []any{}
		if busy.Load() {
			running = []any{"a graph"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"queue_running": running, "queue_pending": []any{},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{Addr: strings.TrimPrefix(srv.URL, "http://"), Probe: true}
	h := &countingHolder{}
	busy.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { HoldWhileBusy(ctx, c, h, 5*time.Millisecond); close(done) }()

	waitFor(t, func() bool { return h.depth() > 0 }, "the busy queue never held the rig")
	busy.Store(false)
	waitFor(t, func() bool { return h.depth() == 0 }, "the hold was never released")

	cancel()
	<-done
	if h.depth() != 0 {
		t.Errorf("hold depth = %d after shutdown, want 0", h.depth())
	}
}

// An unreachable ComfyUI must release the hold rather than extend it: a helper
// whose only move is to keep paying is not the right place to decide what to
// do about a rig in trouble.
func TestAnUnreachableServerDoesNotHoldTheRigForever(t *testing.T) {
	c := &Client{Addr: "127.0.0.1:1", HTTP: &http.Client{Timeout: 50 * time.Millisecond}}
	h := &countingHolder{}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	HoldWhileBusy(ctx, c, h, 10*time.Millisecond)
	if h.depth() != 0 {
		t.Errorf("hold depth = %d, want 0", h.depth())
	}
}

type countingHolder struct {
	mu sync.Mutex
	n  int
}

func (c *countingHolder) EnterInFlight() { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *countingHolder) ExitInFlight()  { c.mu.Lock(); c.n--; c.mu.Unlock() }
func (c *countingHolder) depth() int     { c.mu.Lock(); defer c.mu.Unlock(); return c.n }

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

// ---- output retrieval --------------------------------------------------

// Read from the filesystem rather than from /history: the operator spent the
// session in the browser, queueing renders LARRI never saw.
func TestListReadsTheFilesystemNotTheAPI(t *testing.T) {
	f := &fakeSession{rule: func(cmd string) (string, error) {
		if !strings.Contains(cmd, "find") {
			return "", nil
		}
		return "1024\t1700000000\tComfyUI_00001_.png\n" +
			"2048\t1700000100\tportraits/ComfyUI_00002_.png\n", nil
	}}
	arts, err := List(context.Background(), f, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 2 {
		t.Fatalf("found %d artefacts, want 2", len(arts))
	}
	if arts[1].Rel != "portraits/ComfyUI_00002_.png" {
		t.Errorf("subfolder was flattened: %q", arts[1].Rel)
	}
	if arts[0].Bytes != 1024 {
		t.Errorf("size = %d", arts[0].Bytes)
	}
}

// ComfyUI filenames routinely contain spaces, and splitting on whitespace
// loses them.
func TestListHandlesSpacesInFilenames(t *testing.T) {
	f := &fakeSession{rule: func(cmd string) (string, error) {
		if !strings.Contains(cmd, "find") {
			return "", nil
		}
		return "99\t1700000000\tmy render 01.png\n", nil
	}}
	arts, err := List(context.Background(), f, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 || arts[0].Rel != "my render 01.png" {
		t.Fatalf("artefacts = %+v", arts)
	}
}

func TestSyncSavesRendersLocally(t *testing.T) {
	payload := []byte("\x89PNG\r\n\x1a\nfake image bytes")
	f := &fakeSession{rule: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "find"):
			return fmt.Sprintf("%d\t1700000000\ta.png\n", len(payload)), nil
		case strings.Contains(cmd, "base64"):
			return encodeBase64(payload), nil
		}
		return "", nil
	}}
	dir := t.TempDir()
	res, err := Sync(context.Background(), f, dir, SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete() || len(res.Saved) != 1 {
		t.Fatalf("result = %+v", res)
	}
	got, err := os.ReadFile(filepath.Join(dir, "a.png"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Error("the saved file does not match what the host held")
	}
}

// Session.Run merges stdout and stderr, so a stray warning would corrupt a PNG
// invisibly. The size check is what catches it.
func TestSyncRejectsACorruptedTransfer(t *testing.T) {
	f := &fakeSession{rule: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "find"):
			return "500\t1700000000\ta.png\n", nil
		case strings.Contains(cmd, "base64"):
			return encodeBase64([]byte("too short")), nil
		}
		return "", nil
	}}
	dir := t.TempDir()
	res, err := Sync(context.Background(), f, dir, SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete() {
		t.Fatal("a size mismatch was accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "a.png")); err == nil {
		t.Error("a corrupted file was written under its real name")
	}
}

// The remote filesystem supplies these names, so they are not trusted to stay
// inside the directory they are supposed to.
func TestSyncRefusesAnEscapingRemoteName(t *testing.T) {
	f := &fakeSession{rule: func(cmd string) (string, error) {
		if strings.Contains(cmd, "find") {
			return "10\t1700000000\t../../escaped.png\n", nil
		}
		return encodeBase64([]byte("0123456789")), nil
	}}
	dir := t.TempDir()
	res, err := Sync(context.Background(), f, dir, SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Saved) != 0 {
		t.Error("a path escaping the output directory was written")
	}
	if len(res.Failed) != 1 {
		t.Errorf("the refusal was not reported: %+v", res)
	}
}

// A file already held at the right size is not fetched again, so an adopted
// rig does not re-download a session's whole output.
func TestSyncSkipsWhatIsAlreadyLocal(t *testing.T) {
	payload := []byte("already here")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.png"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	fetched := false
	f := &fakeSession{rule: func(cmd string) (string, error) {
		if strings.Contains(cmd, "find") {
			return fmt.Sprintf("%d\t1700000000\ta.png\n", len(payload)), nil
		}
		fetched = true
		return encodeBase64(payload), nil
	}}
	res, err := Sync(context.Background(), f, dir, SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if fetched {
		t.Error("re-downloaded a file already held locally")
	}
	if len(res.Skipped) != 1 {
		t.Errorf("result = %+v", res)
	}
}

// Since keeps a teardown from re-collecting an earlier session's renders.
func TestSyncHonoursTheSinceCutoff(t *testing.T) {
	f := &fakeSession{rule: func(cmd string) (string, error) {
		if strings.Contains(cmd, "find") {
			return "10\t1000000000\told.png\n10\t2000000000\tnew.png\n", nil
		}
		return encodeBase64([]byte("0123456789")), nil
	}}
	dir := t.TempDir()
	res, err := Sync(context.Background(), f, dir, SyncOptions{
		Since: time.Unix(1_500_000_000, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Saved) != 1 || res.Saved[0] != "new.png" {
		t.Errorf("saved = %v, want only the new render", res.Saved)
	}
}

func TestSummaryReportsLosses(t *testing.T) {
	r := SyncResult{Saved: []string{"a"}, Failed: map[string]string{"b": "boom"}}
	if !strings.Contains(r.Summary(), "FAILED") {
		t.Errorf("a loss is not visible in the summary: %q", r.Summary())
	}
	empty := SyncResult{Failed: map[string]string{}}
	if !strings.Contains(empty.Summary(), "no outputs") {
		t.Errorf("summary = %q", empty.Summary())
	}
}

func encodeBase64(b []byte) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out strings.Builder
	for i := 0; i < len(b); i += 3 {
		var n uint32
		rem := len(b) - i
		n = uint32(b[i]) << 16
		if rem > 1 {
			n |= uint32(b[i+1]) << 8
		}
		if rem > 2 {
			n |= uint32(b[i+2])
		}
		out.WriteByte(chars[(n>>18)&63])
		out.WriteByte(chars[(n>>12)&63])
		if rem > 1 {
			out.WriteByte(chars[(n>>6)&63])
		} else {
			out.WriteByte('=')
		}
		if rem > 2 {
			out.WriteByte(chars[n&63])
		} else {
			out.WriteByte('=')
		}
	}
	return out.String()
}

func stubPort(t *testing.T, s *comfyStub) int {
	t.Helper()
	_, portStr, _ := strings.Cut(strings.TrimPrefix(s.URL, "http://"), ":")
	var port int
	if _, err := fmt.Sscan(portStr, &port); err != nil {
		t.Fatalf("stub port: %v", err)
	}
	return port
}

// The readiness loop calls Ready repeatedly — that is what makes it a wait
// rather than a check. For an engine a repeated call costs a few tokens; here
// it would queue a second render on a GPU billed by the second, for an answer
// already on its way.
func TestReadinessDoesNotQueueDuplicateRenders(t *testing.T) {
	stub := newStub(true, []OutputFile{{Filename: "out.png", Type: "output"}})
	defer stub.Close()

	g, _ := workflow.Parse([]byte(
		`{"1": {"class_type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "a.safetensors"}}}`))
	r := New(g, nil, nil)
	ep := runtime.Endpoint{Host: "127.0.0.1", Port: stubPort(t, stub)}

	for i := 0; i < 4; i++ {
		if err := r.Ready(context.Background(), ep, core.ModelSpec{}); err != nil {
			t.Fatalf("ready %d: %v", i, err)
		}
	}
	if n := stub.submissions(); n != 1 {
		t.Errorf("queued %d renders across four readiness polls, want 1", n)
	}
}

// A graph that failed will fail identically on the next poll, so the
// submission is forgotten rather than re-reported forever.
func TestAFailedReadinessRenderIsNotRemembered(t *testing.T) {
	stub := newStub(true, []OutputFile{{Filename: "out.png", Type: "output"}})
	stub.failWith = "error"
	defer stub.Close()

	g, _ := workflow.Parse([]byte(
		`{"1": {"class_type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "a.safetensors"}}}`))
	r := New(g, nil, nil)
	ep := runtime.Endpoint{Host: "127.0.0.1", Port: stubPort(t, stub)}

	if err := r.Ready(context.Background(), ep, core.ModelSpec{}); err == nil {
		t.Fatal("a graph that finished as an error was accepted as ready")
	}
	r.readyMu.Lock()
	held := r.readyPrompt
	r.readyMu.Unlock()
	if held != "" {
		t.Error("a failed render is still held; every later poll would re-report it")
	}
}

// A live run rented a host and reported "no python with torch" about
// pytorch/pytorch:2.4.0-cuda12.4-cudnn9-runtime — an image that has one. Torch
// lives in conda at /opt/conda/bin, and the PATH that finds it is set by a
// Dockerfile ENV, which applies to `docker run` and not to a command arriving
// over SSH. A non-interactive session sources no profile, so the probe looked
// at a bare /usr/bin PATH and concluded the image was wrong.
func TestPythonIsFoundOutsideThePath(t *testing.T) {
	host := &fakeSession{rule: func(cmd string) (string, error) {
		// Only the conda interpreter exists and only it imports torch,
		// which is the shape of the image that failed.
		if !strings.Contains(cmd, "for p in") {
			return "", nil
		}
		return "FOUND /opt/conda/bin/python\n", nil
	}}
	rt := New(nil, nil, nil)
	err := rt.Bootstrap(context.Background(), host, core.ModelSpec{}, core.SizingPlan{}, nil)
	// Bootstrap goes on to install and fetch, which this stub does not
	// simulate — what matters here is that it got past the probe with the
	// right interpreter.
	if rt.python != "/opt/conda/bin/python" {
		t.Fatalf("python = %q, want the conda interpreter (err: %v)", rt.python, err)
	}
	if err != nil && strings.Contains(err.Error(), "no python with torch") {
		t.Errorf("still reported a missing interpreter: %v", err)
	}
}

// The probe must actually look in the places images put torch, or the fix
// above is one refactor away from regressing.
func TestThePythonProbeSearchesKnownLayouts(t *testing.T) {
	cmd := findPythonCmd()
	for _, want := range []string{
		"python3", "/opt/conda/bin/python", "/opt/venv/bin/python",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("the probe does not look at %s", want)
		}
	}
	// PATH first, so an image that arranged its own environment still wins.
	if strings.Index(cmd, "/opt/conda") < strings.Index(cmd, "'python3'") {
		t.Error("a hard-coded layout is searched before PATH")
	}
}

// "no python with torch" on its own gives an operator who has just paid for a
// rental nothing to act on, and cannot distinguish a missing interpreter from
// a present one whose torch import raises.
func TestAMissingInterpreterCarriesItsEvidence(t *testing.T) {
	host := &fakeSession{rule: func(cmd string) (string, error) {
		if !strings.Contains(cmd, "for p in") {
			return "", nil
		}
		return "NOTFOUND\nsaw /usr/bin/python3\n" +
			"ModuleNotFoundError: No module named 'torch'\n", nil
	}}
	rt := New(nil, nil, nil)
	err := rt.Bootstrap(context.Background(), host, core.ModelSpec{}, core.SizingPlan{}, nil)
	if err == nil {
		t.Fatal("a host with no usable interpreter was accepted")
	}
	msg := err.Error()
	for _, want := range []string{"/usr/bin/python3", "No module named 'torch'"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error drops the evidence %q: %v", want, msg)
		}
	}
}

func TestParsePythonReadsTheProbe(t *testing.T) {
	got, ev := parsePython("FOUND /opt/conda/bin/python\n")
	if got != "/opt/conda/bin/python" || ev != "" {
		t.Errorf("found = %q, evidence = %q", got, ev)
	}
	got, ev = parsePython("NOTFOUND\nsaw /usr/bin/python3\nImportError: libcuda.so.1\n")
	if got != "" {
		t.Errorf("found %q on a NOTFOUND probe", got)
	}
	if !strings.Contains(ev, "libcuda.so.1") {
		t.Errorf("evidence = %q", ev)
	}
}

// ComfyUI's requirements.txt lists torch, torchvision and torchaudio. The
// image already carries a build matched to its CUDA, and letting pip resolve
// them again downloads gigabytes of wheels at the rig's hourly rate to replace
// a working install with one that may not match the driver.
func TestInstallDoesNotReinstallTorch(t *testing.T) {
	var installCmd string
	host := &fakeSession{rule: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "for p in"):
			return "FOUND /opt/conda/bin/python\n", nil
		case strings.Contains(cmd, "main.py") && strings.Contains(cmd, "test -f"):
			return "MISSING\n", nil
		case strings.Contains(cmd, "git clone"):
			installCmd = cmd
			return "INSTALLED\n", nil
		}
		return "", nil
	}}
	rt := New(nil, nil, nil)
	_ = rt.Bootstrap(context.Background(), host, core.ModelSpec{}, core.SizingPlan{}, nil)

	if installCmd == "" {
		t.Fatal("the install step never ran")
	}
	if !strings.Contains(installCmd, "grep -viE") {
		t.Error("torch is not held back; pip would re-resolve it over the image's build")
	}
	if !strings.Contains(installCmd, "requirements.larri.txt") {
		t.Error("pip is not installing from the filtered requirements")
	}
	// The interpreter that has torch is the one pip runs under, not a bare
	// `python3` that may be a different install entirely.
	if !strings.Contains(installCmd, "/opt/conda/bin/python -m pip") {
		t.Errorf("pip ran under the wrong interpreter: %s", installCmd)
	}
}

// A live run installed cleanly, downloaded 6.5 GB of weights, launched, and
// only then died importing a custom op that torch 2.4 cannot parse — roughly
// fifteen billed minutes to learn something an import says in ten seconds.
func TestTheSmokeTestRunsBeforeTheDownload(t *testing.T) {
	var order []string
	host := &fakeSession{rule: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "for p in"):
			return "FOUND /opt/conda/bin/python\n", nil
		case strings.Contains(cmd, "main.py") && strings.Contains(cmd, "test -f"):
			return "MISSING\n", nil
		case strings.Contains(cmd, "git clone"):
			order = append(order, "install")
			return "INSTALLED\n", nil
		case strings.Contains(cmd, "import nodes"):
			order = append(order, "smoke")
			return "SMOKE_OK\n", nil
		case strings.Contains(cmd, "nohup sh"):
			order = append(order, "fetch")
			return "STARTED\n", nil
		case strings.Contains(cmd, "test -f") && strings.Contains(cmd, ".done"):
			return "YES\n", nil
		}
		return "", nil
	}}
	g, _ := workflow.Parse([]byte(
		`{"1": {"class_type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "a.safetensors"}}}`))
	m := &workflow.Manifest{Models: map[string]workflow.Source{
		"a.safetensors": {Repo: "r/a", File: "a.safetensors", Bytes: 10},
	}}
	b, _ := workflow.Resolve(context.Background(), g, nil, workflow.ResolveOptions{Manifest: m})
	rt := New(g, b, map[string]string{"a.safetensors": "https://x.invalid/a"})
	rt.PollInterval = time.Millisecond

	if err := rt.Bootstrap(context.Background(), host, core.ModelSpec{}, core.SizingPlan{}, nil); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	want := []string{"install", "smoke", "fetch"}
	if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Errorf("order = %v, want %v — the check must precede the expensive part", order, want)
	}
}

// And when it fails it must be model-class, so the fallback does not buy the
// same death on two more machines (FR-PROV-05).
func TestAFailedSmokeTestIsNotAHostFailure(t *testing.T) {
	host := &fakeSession{rule: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "for p in"):
			return "FOUND /opt/conda/bin/python\n", nil
		case strings.Contains(cmd, "main.py") && strings.Contains(cmd, "test -f"):
			return "MISSING\n", nil
		case strings.Contains(cmd, "git clone"):
			return "INSTALLED\n", nil
		case strings.Contains(cmd, "import nodes"):
			return "Traceback (most recent call last):\n" +
				"ValueError: infer_schema(func): Parameter stride has unsupported type list[int]\n", nil
		}
		return "", nil
	}}
	rt := New(nil, nil, nil)
	err := rt.Bootstrap(context.Background(), host, core.ModelSpec{}, core.SizingPlan{}, nil)
	if err == nil {
		t.Fatal("a comfyui that cannot import was accepted")
	}
	if errs.ClassOf(err) != errs.ClassModelFailure {
		t.Errorf("class = %v, want model-failure: the next host runs the same "+
			"image and the same ref and dies identically", errs.ClassOf(err))
	}
	if !strings.Contains(err.Error(), "infer_schema") {
		t.Errorf("the error drops the traceback: %v", err)
	}
}

// The same reasoning applied to a death at launch rather than at import: a
// python traceback is a configuration fault, and trying another machine is
// another identical death at another rental's price.
func TestPythonFaultsAreClassifiedAsConfiguration(t *testing.T) {
	rt := New(nil, nil, nil)
	for _, log := range []string{
		"ValueError: infer_schema(func): Parameter stride has unsupported type list[int]",
		"ModuleNotFoundError: No module named 'comfy'",
		"ImportError: libcudart.so.12: cannot open shared object file",
	} {
		if got := rt.ClassifyFailure(log); got != errs.ClassModelFailure {
			t.Errorf("%.40s: class = %v, want model-failure", log, got)
		}
	}
	// Something that really is the machine keeps the host-failure default.
	if got := rt.ClassifyFailure("Killed"); got != errs.ClassUnknown {
		t.Errorf("an oom kill was claimed as a config fault: %v", got)
	}
	if got := runtime.ClassifyFailure(rt, "Killed"); got != errs.ClassHostFailure {
		t.Errorf("no opinion should fall back to host-failure, got %v", got)
	}
}

// Both halves of the pair are pinned, because a bring-up that cannot be
// reproduced cannot be debugged.
func TestTheImageAndTheApplicationAreBothPinned(t *testing.T) {
	rt := New(nil, nil, nil)
	if rt.ComfyUIRef == "" {
		t.Error("the application version is unpinned; --depth 1 of a branch moves daily")
	}
	if !strings.Contains(rt.Image(core.ModelSpec{}, core.SizingPlan{}), ":") {
		t.Error("the image reference names no version at all")
	}
}

// A live run hung here and billed toward the ninety-minute provisioning
// deadline with a working ComfyUI on the host that nothing could reach. sshd
// holds an exec channel open while any live process still has a descriptor on
// it, and stdin was still that channel: the server started, the command never
// returned, and the tunnel was never opened.
func TestLaunchDetachesFromTheSSHChannel(t *testing.T) {
	var launcher string
	host := &fakeSession{rule: func(cmd string) (string, error) {
		if strings.Contains(cmd, "setsid") {
			launcher = cmd
			return "LAUNCHED", nil
		}
		return "", nil
	}}
	rt := New(nil, nil, nil)
	rt.python = "/opt/conda/bin/python"
	if _, err := rt.Launch(context.Background(), host,
		core.ModelSpec{ServedName: "comfyui"}, core.SizingPlan{}); err != nil {
		t.Fatalf("launch: %v", err)
	}

	if !strings.Contains(launcher, "</dev/null") {
		t.Error("stdin still points at the ssh channel; the exec will never return")
	}
	if !strings.Contains(launcher, "setsid") {
		t.Error("the server is not detached from the session")
	}
	// The one that actually mattered. `cd X && setsid ... &` makes the whole
	// compound the asynchronous list, so the shell forks a subshell that
	// inherits the channel's descriptors while only the inner command carries
	// the redirections. That shape hung twice on live hardware; the cd belongs
	// in the script, leaving a single simple command to background.
	if strings.Contains(launcher, "&&") {
		t.Errorf("the launcher backgrounds a compound list: %s", launcher)
	}
	if !strings.Contains(launcher, launchScriptPath) {
		t.Error("the launcher does not run the start script")
	}
	// And the cd has to be somewhere — in the script.
	script := rt.launchScript("/opt/conda/bin/python")
	if !strings.Contains(script, "cd ") || !strings.Contains(script, "exec ") {
		t.Errorf("the start script does not cd and exec: %q", script)
	}
	if !strings.Contains(script, "--listen "+runtime.Loopback) {
		t.Error("the start script does not bind loopback")
	}
}

// A command that produced nothing looks exactly like one that worked, which is
// how a hang was mistaken for a launch.
func TestLaunchFailsWithoutItsConfirmation(t *testing.T) {
	silent := &fakeSession{rule: func(string) (string, error) { return "", nil }}
	rt := New(nil, nil, nil)
	rt.python = "python3"
	_, err := rt.Launch(context.Background(), silent,
		core.ModelSpec{ServedName: "comfyui"}, core.SizingPlan{})
	if err == nil {
		t.Fatal("a launch that confirmed nothing was accepted")
	}
	if !strings.Contains(err.Error(), "no confirmation") {
		t.Errorf("error does not say what was missing: %v", err)
	}
}

// Starting a server is a sub-second operation. There is no legitimate reason
// for that call to be unbounded, and unbounded is what cost a rental.
func TestLaunchIsBounded(t *testing.T) {
	if LaunchTimeout <= 0 || LaunchTimeout > 5*time.Minute {
		t.Errorf("LaunchTimeout = %v, which is not a bound worth having", LaunchTimeout)
	}
	blocked := &fakeSession{rule: func(cmd string) (string, error) {
		if strings.Contains(cmd, "main.py") {
			time.Sleep(2 * time.Second) // outlives the test's own deadline below
		}
		return "", nil
	}}
	rt := New(nil, nil, nil)
	rt.python = "python3"
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_, _ = rt.Launch(ctx, blocked, core.ModelSpec{}, core.SizingPlan{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Launch ignored its context and hung")
	}
}

// The fetch launcher survived the same defect by luck, being a script that
// exits rather than a server that does not.
func TestTheFetchLauncherDetachesToo(t *testing.T) {
	host := &fakeSession{rule: func(cmd string) (string, error) {
		if strings.Contains(cmd, "setsid") {
			return "STARTED\n", nil
		}
		return "", nil
	}}
	d := Download{Dir: ModelsDir, Log: FetchLog, Items: []Item{
		{URL: "u", Kind: workflow.KindCheckpoint, Name: "a.safetensors", Bytes: 1},
	}}
	if err := d.Start(context.Background(), host, false); err != nil {
		t.Fatalf("start: %v", err)
	}
	all := host.all()
	if !strings.Contains(all, "setsid") || !strings.Contains(all, "</dev/null") {
		t.Error("the fetch launcher is still attached to the ssh channel")
	}
}
