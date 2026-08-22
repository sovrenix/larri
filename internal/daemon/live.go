// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/sizing"
	"go.sovrenix.com/larri/internal/sshx"
	"go.sovrenix.com/larri/internal/wire"
)

// Live is a rig that is serving, plus the machinery holding it open.
//
// None of this is persisted, and the ephemeral SSH key is why: FR-STATE-05
// forbids private keys in state files. The consequence is worth naming — a
// process restart cannot currently rebuild the tunnel, only tear the rig down.
// Teardown is a provider API call and never depended on SSH (FR-SEC-18), so
// that is a lost data plane rather than an unkillable bill. Restoring the
// tunnel across a restart needs the key in the OS keyring, which is where
// secrets resolve from anyway (FR-SEC-01).
type Live struct {
	Rig         *core.Rig
	Endpoint    string
	ClientToken secret.Secret

	keys    *sshx.KeyPair
	ssh     *sshx.Client
	forward *sshx.Forward
	proxy   *wire.Proxy
	cancel  context.CancelFunc
}

// Close releases the tunnel and proxy. It does not destroy the instance —
// that is Down's job, and conflating them would make a dropped connection look
// like a teardown.
func (l *Live) Close() error {
	if l.cancel != nil {
		l.cancel()
	}
	if l.forward != nil {
		_ = l.forward.Close()
	}
	if l.ssh != nil {
		_ = l.ssh.Close()
	}
	if l.proxy != nil {
		_ = l.proxy.Close()
	}
	return nil
}

// Serve brings a provisioned rig up to READY: waits for sshd, pins the host
// key, bootstraps, launches, opens the tunnel, and verifies a real completion.
func (o *Orchestrator) Serve(ctx context.Context, rig *core.Rig, keys *sshx.KeyPair,
	localPort int, hfToken secret.Secret) (*Live, error) {

	if rig.Instance == nil {
		return nil, errs.Newf(errs.ClassModelFailure, "daemon.Serve", "rig has no instance")
	}
	live := &Live{Rig: rig, keys: keys}

	// ---- wait for sshd ---------------------------------------------------
	// Reaching sshd is not the same as the host being ready. Vast answers
	// through a shared proxy, so a connection succeeds — and a host key can
	// be read — before the instance's own sshd is listening behind it. The
	// phases below are separate for that reason: an endpoint being reported,
	// a key settling, and a usable session are three different events.
	o.emit("boot", "waiting for the provider to report an ssh endpoint")
	inst, err := o.waitForSSH(ctx, rig)
	if err != nil {
		return live, err
	}
	rig.Instance = inst
	_ = o.Store.Save(rig)
	o.emit("boot", "endpoint %s:%d — settling the host key", inst.SSHHost, inst.SSHPort)

	// ---- pin the host key ------------------------------------------------
	hostKey, client, err := o.pinAndDial(ctx, inst, keys)
	if err != nil {
		return live, err
	}
	rig.HostKeyFingerprint = sshx.Fingerprint(hostKey)
	_ = o.Store.Save(rig)
	o.emit("boot", "host key pinned %s", rig.HostKeyFingerprint)

	live.ssh = client
	sess := client.Session()

	// ---- re-verify sizing against the hardware actually placed -----------
	//
	// The offer described a class; this is the machine. A provider that
	// placed something smaller must be caught before the weights download
	// rather than by an OOM after them.
	if err := o.verifyPlacedHardware(ctx, sess, rig); err != nil {
		return live, err
	}

	// ---- bootstrap and launch -------------------------------------------
	if err := o.Store.Transition(rig, core.StateBootstrapping, "image and weights"); err != nil {
		return live, err
	}
	progress := make(chan runtime.Progress, 16)
	go func() {
		for p := range progress {
			if p.BytesTotal > 0 {
				o.emit("boot", "%s %.0f%% (%s of %s)", p.Phase, p.Percent,
					sizing.HumanBytes(uint64(p.BytesDone)), sizing.HumanBytes(uint64(p.BytesTotal)))
			} else if p.Message != "" {
				o.emit("boot", "%s %s", p.Phase, p.Message)
			}
		}
	}()
	err = o.Runtime.Bootstrap(ctx, sess, rig.Model, rig.Plan, progress)
	close(progress)
	if err != nil {
		return live, err
	}

	o.emit("launch", "starting %s", o.Runtime.Kind())
	ep, err := o.Runtime.Launch(ctx, sess, rig.Model, rig.Plan)
	if err != nil {
		return live, err
	}

	// ---- tunnel ----------------------------------------------------------
	//
	// The listener binds before anything is declared healthy, so a port
	// already in use is an error rather than a rig that reports READY while
	// every client gets connection refused.
	fwd, err := client.Listen(0, ep.Port) // 0: kernel-chosen, proxied below
	if err != nil {
		return live, err
	}
	live.forward = fwd
	tctx, cancel := context.WithCancel(context.Background())
	live.cancel = cancel
	go fwd.Serve(tctx)

	proxy := o.Proxy
	if proxy == nil {
		proxy, err = wire.NewProxy(localPort)
		if err != nil {
			cancel()
			return live, err
		}
		go proxy.Serve(tctx)
	}
	live.proxy = proxy
	proxy.SetUpstream(wire.Upstream{
		Host: "127.0.0.1", Port: fwd.LocalPort(), Key: ep.Key,
	})
	token, err := secret.Generate(32)
	if err != nil {
		cancel()
		return live, err
	}
	proxy.AddClient("larri-cli", token)
	live.ClientToken = token
	rig.LocalPort = proxy.LocalPort()
	live.Endpoint = fmt.Sprintf("http://127.0.0.1:%d/v1", rig.LocalPort)
	o.emit("tunnel", "%s → %s:%d", live.Endpoint, inst.SSHHost, ep.Port)

	// ---- readiness, through the tunnel ----------------------------------
	//
	// Checked at the LOCAL end rather than on the host, so success proves the
	// whole path: the forward carries traffic, the proxy substitutes the rig
	// credential, and the model produces a token. A check run on the host
	// would prove only that vLLM answers itself.
	o.emit("ready", "waiting for a completion to round-trip")
	if err := o.waitReady(ctx, rig, proxy.LocalPort(), token); err != nil {
		return live, err
	}
	if err := o.Store.Transition(rig, core.StateReady, "completion round-trip verified"); err != nil {
		return live, err
	}
	return live, nil
}

// waitForSSH waits for the host to become usable, driven by what the provider
// says it is doing rather than by a blind clock.
//
// A live run made the case for this. Three attempts each died at a six-minute
// deadline while the host was still pulling the runtime image — a stock vLLM
// image is 10-15 GB, and Vast reports that phase as "loading". Every timeout
// threw away a partly-finished pull and started the same one on a fresh
// machine, so the run spent eighteen minutes of billing and kept no progress
// at all. The deadline was not protecting the operator; it was burning their
// money on principle.
//
// So the wait is **progress-driven**: as long as the provider's status keeps
// changing, the host is working and LARRI keeps waiting. What ends it is a
// *stall* — no change for StallTimeout — which is the actual signal that a
// machine has stopped trying. An overall cap remains, because a host that
// reports novel status messages forever is still a host that is not serving.
//
// It also reports each change, so a long boot looks like progress rather than
// like a hang (FR-RT-06 applied to the phase before the runtime exists).
func (o *Orchestrator) waitForSSH(ctx context.Context, rig *core.Rig) (*core.Instance, error) {
	stall := o.BootStallTimeout
	if stall == 0 {
		stall = 8 * time.Minute
	}
	cap := o.BootCap
	if cap == 0 {
		cap = 30 * time.Minute
	}
	poll := o.BootPollInterval
	if poll == 0 {
		poll = 10 * time.Second
	}
	var (
		lastSeen  string
		changedAt = time.Now()
		deadline  = time.Now().Add(cap)
	)
	for time.Now().Before(deadline) {
		inst, err := o.Provider.Get(ctx, rig.Instance.InstanceID)
		switch {
		case err != nil:
			// Unreachable is not absent; keep asking (FR-SUP-11). A provider
			// outage must not be mistaken for a dead host.
			o.warn("boot", "status query failed: %v", err)
		case inst == nil:
			return nil, errs.Newf(errs.ClassProviderUnknownOutcome, "daemon.waitForSSH",
				"instance %s vanished during boot", rig.Instance.InstanceID)
		case inst.Running && inst.SSHHost != "" && inst.SSHPort > 0:
			return inst, nil
		default:
			if now := describeBoot(inst); now != lastSeen {
				o.emit("boot", "%s", now)
				lastSeen, changedAt = now, time.Now()
				o.lastBootStatus = now
			}
		}
		if idle := time.Since(changedAt); idle > stall {
			return nil, errs.Newf(errs.ClassHostFailure, "daemon.waitForSSH",
				"no progress for %s (last: %s)", idle.Round(time.Second), orUnknown(lastSeen))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
	return nil, errs.Newf(errs.ClassHostFailure, "daemon.waitForSSH",
		"host did not become usable within %s (last: %s)", cap, orUnknown(lastSeen))
}

// describeBoot renders the provider's account of what a host is doing.
func describeBoot(inst *core.Instance) string {
	status := inst.Status
	if status == "" {
		status = "starting"
	}
	if inst.StatusMsg != "" {
		msg := inst.StatusMsg
		if len(msg) > 120 {
			msg = msg[:120]
		}
		return status + ": " + msg
	}
	if inst.SSHHost == "" {
		return status + " (no ssh endpoint yet)"
	}
	return status
}

func orUnknown(s string) string {
	if s == "" {
		return "no status reported"
	}
	return s
}

// verifyPlacedHardware re-runs the fit check against the machine that was
// actually provisioned.
func (o *Orchestrator) verifyPlacedHardware(ctx context.Context, sess runtime.Session, rig *core.Rig) error {
	out, err := sess.Run(ctx,
		"nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits 2>/dev/null || true")
	if err != nil || len(out) == 0 {
		// Telemetry is subordinate (T1): failing to read the GPU must not
		// fail a rig. The sizing plan already passed against the offer.
		o.warn("boot", "could not read GPU memory to re-verify sizing")
		return nil
	}
	totalMB := parseFirstInt(string(out))
	if totalMB <= 0 {
		return nil
	}
	haveBytes := uint64(totalMB) * 1024 * 1024
	if haveBytes < rig.Plan.RequiredVRAMBytes {
		return errs.Newf(errs.ClassHostFailure, "daemon.verifyPlacedHardware",
			"placed hardware has %s but the plan needs %s",
			sizing.HumanBytes(haveBytes), sizing.HumanBytes(rig.Plan.RequiredVRAMBytes))
	}
	o.emit("boot", "GPU reports %s, plan needs %s",
		sizing.HumanBytes(haveBytes), sizing.HumanBytes(rig.Plan.RequiredVRAMBytes))
	return nil
}

func (o *Orchestrator) waitReady(ctx context.Context, rig *core.Rig, port int, token secret.Secret) error {
	deadline := time.Now().Add(20 * time.Minute)
	var last error
	attempts := 0
	for time.Now().Before(deadline) {
		attempts++
		ep := runtime.Endpoint{Host: "127.0.0.1", Port: port, Model: rig.Model.ServedName, Key: token}
		if err := o.Runtime.Ready(ctx, ep, rig.Model); err == nil {
			o.emit("ready", "completion verified after %d attempts", attempts)
			return nil
		} else {
			last = err
		}
		if attempts%6 == 0 {
			o.emit("ready", "still loading (%v)", shortErr(last))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return errs.Newf(errs.ClassHostFailure, "daemon.waitReady",
		"no completion returned before the deadline: %v", shortErr(last))
}

func parseFirstInt(s string) int {
	n, seen := 0, false
	for _, r := range s {
		if r >= '0' && r <= '9' {
			n = n*10 + int(r-'0')
			seen = true
		} else if seen {
			break
		}
	}
	return n
}

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// pinAndDial establishes a host key and connects with it pinned.
//
// A live run exposed why this cannot be scan-once-then-dial. Vast reaches
// instances through a shared proxy (`ssh6.vast.ai`), and the proxy answers
// before the instance's own sshd is listening — so a single scan can capture
// one key and the connection a second later present another, which arrives as
// `host key mismatch` on a machine nobody has attacked.
//
// The resolution keeps the security property while accounting for the
// handover: **the key must be observed twice, agreeing, before it is pinned**,
// and a mismatch is tolerated *only* during initial bring-up, where it means
// the far end changed hands rather than that someone intervened.
//
// This is honest about what it costs. It widens the trust-on-first-use window
// from an instant to the seconds between scans, and §15.8.2 already states the
// larger limit — the fingerprint comes from a path the provider controls, so
// pinning cannot exclude the provider in any case. What it does buy is
// unchanged: a third party on the network cannot substitute themselves, and
// once a rig is serving, any key change is a compromise rather than a race.
func (o *Orchestrator) pinAndDial(ctx context.Context, inst *core.Instance,
	keys *sshx.KeyPair) (ssh.PublicKey, *sshx.Client, error) {

	const attempts = 8
	var lastErr error
	for i := 0; i < attempts; i++ {
		key, err := o.stableHostKey(ctx, inst)
		if err != nil {
			lastErr = err
		} else {
			client, derr := sshx.Dial(ctx, sshx.Config{
				Host: inst.SSHHost, Port: inst.SSHPort, User: "root",
				Key: keys, HostKey: key, Timeout: 60 * time.Second,
			})
			if derr == nil {
				return key, client, nil
			}
			lastErr = derr
			if !isHostKeyMismatch(derr) {
				// Anything other than a mismatch is a real failure: a refused
				// connection, a rejected credential, a broken host.
				return nil, nil, derr
			}
			o.emit("boot", "host key changed during boot; re-pinning")
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return nil, nil, errs.Newf(errs.ClassHostFailure, "daemon.pinAndDial",
		"host key never settled: %v", shortErr(lastErr))
}

// stableHostKey returns a key only once two consecutive observations agree.
func (o *Orchestrator) stableHostKey(ctx context.Context, inst *core.Instance) (ssh.PublicKey, error) {
	first, err := sshx.ScanHostKey(ctx, inst.SSHHost, inst.SSHPort, 45*time.Second)
	if err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(3 * time.Second):
	}
	second, err := sshx.ScanHostKey(ctx, inst.SSHHost, inst.SSHPort, 45*time.Second)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(first.Marshal(), second.Marshal()) {
		return nil, errs.Newf(errs.ClassHostFailure, "daemon.stableHostKey",
			"host key still changing")
	}
	return second, nil
}

func isHostKeyMismatch(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "host key mismatch")
}
