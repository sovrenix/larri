// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/provider"
	rfake "go.sovrenix.com/larri/internal/runtime/fake"
	"go.sovrenix.com/larri/internal/secret"
)

// bootProvider reports a scripted sequence of statuses, so the wait can be
// tested against how a host actually behaves rather than against a stub that
// either works or does not.
type bootProvider struct {
	mu     sync.Mutex
	steps  []core.Instance
	calls  int
	repeat bool // keep returning the last step forever
}

func (b *bootProvider) Name() string { return "boot" }
func (b *bootProvider) Search(context.Context, core.Criteria) ([]core.Offer, error) {
	return nil, nil
}
func (b *bootProvider) Create(context.Context, core.Offer, provider.CreateSpec) (*core.Instance, error) {
	return nil, nil
}
func (b *bootProvider) List(context.Context) ([]core.Instance, error) { return nil, nil }
func (b *bootProvider) Destroy(context.Context, string) error         { return nil }

func (b *bootProvider) Get(context.Context, string) (*core.Instance, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	i := b.calls
	b.calls++
	if i >= len(b.steps) {
		if !b.repeat || len(b.steps) == 0 {
			return nil, fmt.Errorf("no more steps")
		}
		last := b.steps[len(b.steps)-1]
		return &last, nil
	}
	step := b.steps[i]
	return &step, nil
}

func loading(msg string) core.Instance {
	return core.Instance{InstanceID: "i", Status: "loading", StatusMsg: msg}
}

// The failure a live run produced three times: a fixed deadline killed a host
// that was making steady progress pulling a 15 GB image, threw the partial
// pull away, and started the same one on a fresh machine.
func TestSlowButProgressingBootIsNotKilled(t *testing.T) {
	// The final step advertises an endpoint that actually answers, because
	// readiness is now decided by the connection rather than by the provider's
	// label — a host that says "running" against a dead port is the failure
	// TestAdvertisedEndpointThatNeverAnswersIsAHostFailure covers.
	port := newLocalSSHIsh(t)
	p := &bootProvider{steps: []core.Instance{
		loading("Downloading vllm/vllm-openai 12%"),
		loading("Downloading vllm/vllm-openai 34%"),
		loading("Downloading vllm/vllm-openai 61%"),
		loading("Downloading vllm/vllm-openai 88%"),
		loading("Extracting layers"),
		{InstanceID: "i", Status: "running", Running: true, SSHHost: "127.0.0.1", SSHPort: port},
	}}
	var events []string
	ch := make(chan Event, 64)
	done := make(chan struct{})
	go func() {
		for e := range ch {
			events = append(events, e.Message)
		}
		close(done)
	}()

	o := &Orchestrator{
		Provider: p, Events: ch,
		BootStallTimeout: 2 * time.Second, // a change every poll keeps this from firing
		BootCap:          time.Minute,
		BootPollInterval: 20 * time.Millisecond,
	}
	rig := &core.Rig{Instance: &core.Instance{InstanceID: "i"}}
	inst, err := o.waitForSSH(context.Background(), rig)
	close(ch)
	<-done

	if err != nil {
		t.Fatalf("a boot that keeps progressing must not be killed: %v", err)
	}
	if inst == nil || !inst.Running {
		t.Fatal("should have reached a running instance")
	}
	// Progress is reported, so a long boot reads as work rather than a hang.
	var sawPercent bool
	for _, e := range events {
		if strings.Contains(e, "%") {
			sawPercent = true
		}
	}
	if !sawPercent {
		t.Errorf("the provider's own progress should be surfaced, got %v", events)
	}
}

// A stall is the real signal that a host gave up, where a fixed deadline only
// measures how large an image happens to be.
func TestStalledBootIsAHostFailure(t *testing.T) {
	p := &bootProvider{
		steps:  []core.Instance{loading("Downloading 4%")},
		repeat: true, // the same message forever
	}
	o := &Orchestrator{
		Provider:         p,
		BootStallTimeout: 300 * time.Millisecond,
		BootCap:          time.Minute,
		BootPollInterval: 20 * time.Millisecond,
	}
	rig := &core.Rig{Instance: &core.Instance{InstanceID: "i"}}
	start := time.Now()
	_, err := o.waitForSSH(context.Background(), rig)
	if err == nil {
		t.Fatal("a host that stopped progressing must fail")
	}
	if !errs.Is(err, errs.ClassHostFailure) {
		t.Fatalf("class = %s, want host-failure so it earns a fallback", errs.ClassOf(err))
	}
	if !strings.Contains(err.Error(), "no progress") {
		t.Errorf("the error should name the stall: %v", err)
	}
	// It must name what it was last doing, or the operator cannot tell a
	// stuck pull from a stuck scheduler.
	if !strings.Contains(err.Error(), "Downloading 4%") {
		t.Errorf("the last status should be reported: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("stall detection should end the wait promptly")
	}
}

// A provider outage is not a dead host: keep asking rather than concluding
// (FR-SUP-11).
func TestQueryFailureDoesNotEndTheWait(t *testing.T) {
	p := &bootProvider{steps: []core.Instance{}} // Get always errors
	o := &Orchestrator{
		Provider:         p,
		BootStallTimeout: 400 * time.Millisecond,
		BootCap:          5 * time.Second,
		BootPollInterval: 20 * time.Millisecond,
	}
	rig := &core.Rig{Instance: &core.Instance{InstanceID: "i"}}
	_, err := o.waitForSSH(context.Background(), rig)
	if err == nil {
		t.Fatal("it should eventually give up")
	}
	// But not by concluding the instance is gone.
	if errs.Is(err, errs.ClassProviderUnknownOutcome) {
		t.Error("a failed query must not be read as the instance vanishing")
	}
	if p.calls < 2 {
		t.Errorf("only %d queries; a transient failure must be retried", p.calls)
	}
}

func TestDescribeBootPrefersTheProviderMessage(t *testing.T) {
	if got := describeBoot(&core.Instance{Status: "loading", StatusMsg: "pull 40%"}); got != "loading: pull 40%" {
		t.Errorf("got %q", got)
	}
	if got := describeBoot(&core.Instance{Status: "loading"}); !strings.Contains(got, "no ssh endpoint") {
		t.Errorf("got %q", got)
	}
	// A very long provider message must not flood the output.
	long := describeBoot(&core.Instance{Status: "loading", StatusMsg: strings.Repeat("x", 500)})
	if len(long) > 200 {
		t.Errorf("message not truncated: %d chars", len(long))
	}
}

// Silence from a provider is not evidence of a stalled host.
//
// Vast reports contract state the moment billing starts but often says nothing
// about the container for minutes while an image pulls — no status, no
// message. Treating that silence as a stall kills a host that is working,
// which is the same mistake a fixed deadline makes wearing a smarter disguise.
func TestProviderSilenceIsNotAStall(t *testing.T) {
	quiet := core.Instance{InstanceID: "i", Status: "contract running"} // no StatusMsg, ever
	p := &bootProvider{steps: []core.Instance{quiet}, repeat: true}
	o := &Orchestrator{
		Provider:         p,
		BootStallTimeout: 100 * time.Millisecond, // would fire instantly if applied
		BootCap:          1200 * time.Millisecond,
		BootPollInterval: 20 * time.Millisecond,
	}
	rig := &core.Rig{Instance: &core.Instance{InstanceID: "i"}}
	start := time.Now()
	_, err := o.waitForSSH(context.Background(), rig)
	if err == nil {
		t.Fatal("it should eventually give up on the cap")
	}
	if strings.Contains(err.Error(), "no progress") {
		t.Fatal("a silent provider was reported as a stalled host")
	}
	// It waited for the cap, not the much shorter stall window.
	if time.Since(start) < time.Second {
		t.Errorf("gave up after %s; the cap should govern when there is no signal",
			time.Since(start).Round(time.Millisecond))
	}
}

// Once the provider HAS spoken, going quiet is a genuine stall.
func TestStallAppliesOnceTheProviderHasSpoken(t *testing.T) {
	p := &bootProvider{
		steps:  []core.Instance{loading("Downloading 4%")},
		repeat: true,
	}
	o := &Orchestrator{
		Provider:         p,
		BootStallTimeout: 200 * time.Millisecond,
		BootCap:          10 * time.Second,
		BootPollInterval: 20 * time.Millisecond,
	}
	rig := &core.Rig{Instance: &core.Instance{InstanceID: "i"}}
	_, err := o.waitForSSH(context.Background(), rig)
	if err == nil || !strings.Contains(err.Error(), "no progress") {
		t.Fatalf("a host that spoke and then went quiet is stalled: %v", err)
	}
}

// The failure a live run produced and neither earlier rule caught.
//
// The contract reported running for ten minutes, intended_status was running,
// actual_status never arrived, status_msg stayed empty — and the advertised
// endpoint refused every connection. The fixed deadline would have waited six
// minutes for nothing; the stall clock could not fire because the provider had
// never spoken; the cap would have billed for thirty. The connection knew all
// along.
func TestAdvertisedEndpointThatNeverAnswersIsAHostFailure(t *testing.T) {
	// A port with nothing behind it, which is what the live instance was.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	p := &bootProvider{repeat: true, steps: []core.Instance{{
		InstanceID: "i", Status: "contract running", // never "running"
		SSHHost: "127.0.0.1", SSHPort: dead,
	}}}
	o := &Orchestrator{
		Provider:           p,
		BootPollInterval:   20 * time.Millisecond,
		EndpointStallLimit: 500 * time.Millisecond,
		BootCap:            30 * time.Second,
		BootStallTimeout:   30 * time.Second,
	}
	rig := &core.Rig{Instance: &core.Instance{InstanceID: "i"}}
	start := time.Now()
	_, err = o.waitForSSH(context.Background(), rig)
	if err == nil {
		t.Fatal("an endpoint that never answers is a dead host")
	}
	if !errs.Is(err, errs.ClassHostFailure) {
		t.Fatalf("class = %s, want host-failure so it earns a fallback", errs.ClassOf(err))
	}
	if !strings.Contains(err.Error(), "still not answering") {
		t.Errorf("the error should name what was tried: %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("took %s; the point is to stop billing promptly", time.Since(start))
	}
}

// The converse, and why the probe replaces the status check rather than
// supplementing it: a reachable host is usable even when the provider has not
// got round to relabelling it.
func TestReachableEndpointWinsOverAnUnsetStatus(t *testing.T) {
	srv := newLocalSSHIsh(t)
	p := &bootProvider{repeat: true, steps: []core.Instance{{
		InstanceID: "i",
		Status:     "contract running", // Running is false; actual_status never arrived
		SSHHost:    "127.0.0.1", SSHPort: srv,
	}}}
	o := &Orchestrator{
		Provider: p, BootPollInterval: 20 * time.Millisecond,
		EndpointStallLimit: 10 * time.Second, BootCap: 10 * time.Second,
	}
	rig := &core.Rig{Instance: &core.Instance{InstanceID: "i"}}
	inst, err := o.waitForSSH(context.Background(), rig)
	if err != nil {
		t.Fatalf("a host that answers is usable regardless of its label: %v", err)
	}
	if inst == nil {
		t.Fatal("should have returned the instance")
	}
}

// newLocalSSHIsh starts a listener that sends an SSH banner, standing in for a
// container whose sshd is up before the provider has noticed.
func newLocalSSHIsh(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
			c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// logSession scripts a runtime log and host counters, so the two readiness
// regimes can be tested against how a bring-up actually behaves.
type logSession struct {
	mu      sync.Mutex
	size    int64
	growBy  int64
	cpuBusy uint64
	calls   int
}

func (s *logSession) Run(_ context.Context, cmd string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	switch {
	case strings.Contains(cmd, "stat -c"):
		s.size += s.growBy
		return []byte(fmt.Sprintf("%d\nloading weights\n", s.size)), nil
	case strings.Contains(cmd, "/proc/stat"):
		s.cpuBusy += 10
		return []byte(fmt.Sprintf("cpu 1000 900\ndisk 0 0\nnet 0 0\n")), nil
	}
	return nil, nil
}
func (s *logSession) Dial(context.Context, int) (io.ReadWriteCloser, error) { return nil, nil }
func (s *logSession) Close() error                                          { return nil }

// A runtime that produces nothing and a host that does nothing is answered
// quickly: before there is a single log line there is nothing to be patient
// about.
func TestColdStartGivesUpQuickly(t *testing.T) {
	o := &Orchestrator{
		Runtime:           rfake.New(rfake.Behaviour{NeverReady: true}),
		ColdStartLimit:    300 * time.Millisecond,
		WarmStallLimit:    time.Hour, // must not be what fires
		ReadyCap:          20 * time.Second,
		ReadyPollInterval: 50 * time.Millisecond,
	}
	rig := &core.Rig{Model: core.ModelSpec{ServedName: "m"}}
	sess := &logSession{growBy: 0} // log never grows

	start := time.Now()
	err := o.waitReady(context.Background(), sess, rig, 1, secret.New("t"))
	if err == nil {
		t.Fatal("a silent runtime on an idle host should fail")
	}
	if !strings.Contains(err.Error(), "no output") {
		t.Errorf("the error should say the runtime never spoke: %v", err)
	}
	if time.Since(start) > 15*time.Second {
		t.Errorf("took %s; the cold regime exists to stop billing early", time.Since(start))
	}
}

// Once output starts the calculus inverts: a weight download is legitimately
// slow, and killing it wastes everything already transferred.
func TestWarmRegimeIsPatientWhileTheLogGrows(t *testing.T) {
	o := &Orchestrator{
		Runtime:           rfake.New(rfake.Behaviour{NeverReady: true}),
		ColdStartLimit:    100 * time.Millisecond, // would fire instantly if it applied
		WarmStallLimit:    30 * time.Second,
		ReadyCap:          2500 * time.Millisecond,
		ReadyPollInterval: 50 * time.Millisecond,
	}
	rig := &core.Rig{Model: core.ModelSpec{ServedName: "m"}}
	sess := &logSession{growBy: 4096} // steadily writing

	start := time.Now()
	err := o.waitReady(context.Background(), sess, rig, 1, secret.New("t"))
	if err == nil {
		t.Fatal("it should eventually hit the cap")
	}
	// It must have waited for the cap, not the much shorter cold limit.
	if time.Since(start) < 2*time.Second {
		t.Errorf("gave up after %s; a growing log means work is happening",
			time.Since(start).Round(time.Millisecond))
	}
	if strings.Contains(err.Error(), "no output") {
		t.Error("a runtime that produced output was reported as silent")
	}
}
