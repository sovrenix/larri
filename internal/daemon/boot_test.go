// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/provider"
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
	p := &bootProvider{steps: []core.Instance{
		loading("Downloading vllm/vllm-openai 12%"),
		loading("Downloading vllm/vllm-openai 34%"),
		loading("Downloading vllm/vllm-openai 61%"),
		loading("Downloading vllm/vllm-openai 88%"),
		loading("Extracting layers"),
		{InstanceID: "i", Status: "running", Running: true, SSHHost: "h", SSHPort: 22},
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
