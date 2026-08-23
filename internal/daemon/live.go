// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/deadman"
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
// forbids private keys in state files. That is not a limitation to work
// around; it is the property Adopt is built on. A restart recovers the rig by
// minting a new key and having the provider install it, so the key that was
// lost is superseded rather than retrieved — and there is no keyring entry to
// steal, expire, or forget to clean up.
//
// The instance itself was never at risk either way: teardown is a provider API
// call and has never depended on SSH (FR-SEC-18).
type Live struct {
	Rig         *core.Rig
	Endpoint    string
	ClientToken secret.Secret

	keys    *sshx.KeyPair
	ssh     *sshx.Client
	beating context.CancelFunc
	forward *sshx.Forward
	proxy   *wire.Proxy
	cancel  context.CancelFunc
}

// Close releases the tunnel and proxy. It does not destroy the instance —
// that is Down's job, and conflating them would make a dropped connection look
// like a teardown.
func (l *Live) Close() error {
	// The heartbeat stops first. From here the host is on its own clock,
	// which is the entire point: whatever killed this process cannot also
	// have stopped the watchdog.
	if l.beating != nil {
		l.beating()
	}
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

	// ---- arm the dead-man switch ----------------------------------------
	//
	// Before the bootstrap, not after it, because the image pull and weight
	// download are the longest and most expensive window there is — and the
	// one where a killed LARRI leaves the biggest bill. The host is armed the
	// moment it can be told anything at all.
	if o.DeadmanDeadline >= 0 {
		cfg := deadman.Config{
			Deadline:    o.deadmanDeadline(),
			RuntimePort: o.runtimePort(),
			RuntimeLog:  o.runtimeLogPath(),
		}
		if err := deadman.Arm(ctx, sess, cfg); err != nil {
			// Not fatal. A rig that serves without a backstop is worse than
			// one with it and better than none at all — but the operator is
			// told, because they may be relying on it.
			o.warn("deadman", "could not arm the host watchdog: %s", shortErr(err))
		} else {
			o.emit("deadman", "host will stop serving if larri goes quiet for %s "+
				"(containment, not a teardown — destroy to stop the bill)",
				cfg.Deadline.Round(time.Minute))
			live.beating = o.startBeating(sess)
		}
	}

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

	// Once SSH is up, the host can be asked directly whether it is working,
	// which is a far better signal than the provider's status text. Any of
	// CPU, disk, or network moving counts: an image finishing its download and
	// starting to extract goes network-quiet and disk-busy, and a probe that
	// watched only the wire would call that stuck.
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go o.watchHostActivity(watchCtx, sess, o.hostProbeInterval(), func(idle time.Duration) {
		if idle > o.hostIdleLimit() {
			o.warn("host", "no CPU, disk or network activity for %s", idle.Round(time.Second))
		}
	})
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

	// Hand the weight-download credential over at launch, so it never reaches
	// a snapshot or a journal entry (FR-STATE-05).
	if taker, ok := o.Runtime.(runtime.CredentialTaker); ok && !hfToken.Empty() {
		taker.SetHuggingFaceToken(hfToken)
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
	if err := o.attachTunnel(ctx, live, rig, ep, localPort); err != nil {
		return live, err
	}
	o.emit("tunnel", "%s → %s:%d", live.Endpoint, inst.SSHHost, ep.Port)

	// ---- readiness, through the tunnel ----------------------------------
	//
	// Checked at the LOCAL end rather than on the host, so success proves the
	// whole path: the forward carries traffic, the proxy substitutes the rig
	// credential, and the model produces a token. A check run on the host
	// would prove only that vLLM answers itself.
	o.emit("ready", "waiting for a completion to round-trip")
	if err := o.waitReady(ctx, sess, rig, live.proxy.LocalPort(), live.ClientToken); err != nil {
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
		poll = 15 * time.Second
	}
	unreachable := o.EndpointStallLimit
	if unreachable == 0 {
		unreachable = 3 * time.Minute
	}
	var (
		lastSeen    string
		changedAt   = time.Now()
		deadline    = time.Now().Add(cap)
		everSpoke   bool
		endpointAt  time.Time
		announcedEP bool
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

		case inst.SSHHost != "" && inst.SSHPort > 0:
			// The endpoint is published. From here the provider's status is
			// advisory and the connection is decisive: a live run watched a
			// contract report "running" for ten minutes, with intended_status
			// running and actual_status never set, against an address nothing
			// was listening on. Asking the address costs the provider nothing
			// and answers the only question that matters (§12.1 — resolve by
			// evidence, not inference).
			if endpointAt.IsZero() {
				endpointAt = time.Now()
			}
			if !announcedEP {
				o.emit("boot", "endpoint %s:%d published — probing", inst.SSHHost, inst.SSHPort)
				announcedEP = true
			}
			if perr := sshx.Probe(ctx, inst.SSHHost, inst.SSHPort, 15*time.Second); perr == nil {
				o.emit("boot", "sshd answering at %s:%d", inst.SSHHost, inst.SSHPort)
				return inst, nil
			} else if waited := time.Since(endpointAt); waited > unreachable {
				// Measured from the last sign of life, not from publication.
				// See the reset below.
				return nil, errs.Newf(errs.ClassHostFailure, "daemon.waitForSSH",
					"endpoint %s:%d unreachable for %s while nothing changed: %v",
					inst.SSHHost, inst.SSHPort, waited.Round(time.Second), shortErr(perr))
			}
			fallthrough

		default:
			if inst != nil && inst.StatusMsg != "" {
				everSpoke = true
			}
			if inst != nil {
				if now := describeBoot(inst); now != lastSeen {
					o.emit("boot", "%s", now)
					lastSeen, changedAt = now, time.Now()
					o.lastBootStatus = now

					// The endpoint clock is progress-driven too, and for the
					// same reason the status clock is. A live run on a GTX
					// 1050 Ti killed a host 106 seconds after its endpoint
					// appeared while the provider was reporting "Verifying
					// Checksum" and "Pull complete" — sshd was not up because
					// the image was still arriving, which is a host working,
					// not a host dead. Cheap cards pull slowly, so the
					// engines that exist to use cheap cards hit this most.
					//
					// Resetting here does not weaken what the limit was built
					// to catch. That failure is a contract reporting running
					// with nothing listening, and its status does not change
					// either — so the clock still runs out on it.
					if !endpointAt.IsZero() {
						endpointAt = time.Now()
					}
				}
			}
		}
		// Stall detection needs something to detect a stall IN.
		//
		// Vast reports contract state immediately but often says nothing about
		// the container for minutes — no status, no message — while an image
		// pulls. Treating that silence as a stall would kill a host that is
		// working, which is the same mistake the fixed deadline made wearing a
		// smarter disguise. So the stall clock only applies once the provider
		// has actually shown progress; the endpoint probe above is what
		// catches a host that never comes up at all.
		if everSpoke {
			if idle := time.Since(changedAt); idle > stall {
				return nil, errs.Newf(errs.ClassHostFailure, "daemon.waitForSSH",
					"no progress for %s (last: %s)",
					idle.Round(time.Second), orUnknown(lastSeen))
			}
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

// waitReady waits for a real completion, in two regimes.
//
// Once SSH is up LARRI has far better control than the provider's status text
// allowed, and the wait should reflect that. Before the runtime has produced a
// single line of output there is nothing to be patient about: either the
// process died on launch or the host is doing nothing, and both are answered
// in minutes by the log being empty and the hardware idle. Once output starts,
// the calculus inverts — a weight download is legitimately slow, so patience
// should be generous while the log keeps growing or the disk keeps moving.
//
// The signals are independent on purpose. A log that is growing proves work
// even if the counters are quiet; hardware that is busy proves work even
// through a phase that logs nothing. Requiring both would reproduce the
// single-signal blindness that has already cost four bring-ups.
func (o *Orchestrator) waitReady(ctx context.Context, sess runtime.Session,
	rig *core.Rig, port int, token secret.Secret) error {

	cold := o.ColdStartLimit
	if cold == 0 {
		cold = 4 * time.Minute
	}
	warm := o.WarmStallLimit
	if warm == 0 {
		warm = 12 * time.Minute
	}
	deadline := time.Now().Add(o.readyCap())

	var (
		logBytes   int64
		lastGrowth = time.Now()
		everLogged bool
		prevCount  = readCounters(ctx, sess)
		attempts   int
	)
	for time.Now().Before(deadline) {
		attempts++
		ep := runtime.Endpoint{Host: "127.0.0.1", Port: port,
			Model: rig.Model.ServedName, Key: token}
		if err := o.Runtime.Ready(ctx, ep, rig.Model); err == nil {
			o.emit("ready", "completion verified after %s",
				time.Since(deadline.Add(-o.readyCap())).Round(time.Second))
			return nil
		}

		size, tail := o.readLogState(ctx, sess)
		cur := readCounters(ctx, sess)
		act := cur.since(prevCount)
		prevCount = cur

		grew := size > logBytes
		if grew {
			if !everLogged {
				o.emit("ready", "runtime started producing output")
			}
			everLogged = true
			logBytes = size
			lastGrowth = time.Now()
			if tail != "" {
				o.emit("ready", "%s", tail)
			}
		} else if act.Moving {
			// The log can go quiet mid-phase while the machine is plainly
			// working: loading a checkpoint into VRAM writes nothing.
			lastGrowth = time.Now()
		}
		if attempts%4 == 0 {
			o.emit("ready", "log %s · %s", humanSize(logBytes), act)
		}

		// A runtime that has exited is not a runtime that is being slow. The
		// stall timeout below is deliberately patient — a large checkpoint
		// loads for many minutes in silence — and that patience is billed. It
		// should never be spent on a process that is already gone.
		//
		// Consulted when the log has stopped growing, and gated on nothing
		// else. Hardware activity deliberately does not veto it: /proc on a
		// marketplace instance reports the whole machine (§12.2.3), so the
		// CPU it shows is mostly other tenants and says nothing about our
		// process. A live run sat for minutes at "cpu 9%" while llama-server
		// had already exited on a missing shared library — the counters were
		// real, the work was somebody else's.
		//
		// The log is different: it is our process's own output, so a growing
		// log outranks the probe and still does.
		//
		// It stays meaningless before the runtime has spoken: an absent
		// process may simply not have started yet.
		if everLogged && !grew {
			if lc, ok := o.Runtime.(runtime.LivenessChecker); ok {
				if alive, err := lc.Alive(ctx, sess); err == nil && !alive {
					// "quiet for" rather than "exited after": the
					// duration is how long since the log last moved, not
					// how long the runtime ran. An operator reading
					// "exited after 12s" would hunt for a crash on
					// startup that never happened.
					return errs.Newf(errs.ClassHostFailure, "daemon.waitReady",
						"runtime exited without serving; log quiet for %s%s",
						idleSince(lastGrowth), o.runtimeSaid(ctx, sess))
				}
			}
		}

		idle := time.Since(lastGrowth)
		switch {
		case !everLogged && idle > cold:
			// A runtime that never spoke is usually a runtime that died on
			// its first line — a missing library, an unsupported flag, a
			// wrong architecture. Whatever it managed to say is the answer,
			// so it goes in the error rather than being left on the host that
			// is about to be destroyed.
			return errs.Newf(errs.ClassHostFailure, "daemon.waitReady",
				"runtime produced no output in %s and the host is idle (%s)%s",
				idle.Round(time.Second), act, o.runtimeSaid(ctx, sess))
		case everLogged && idle > warm:
			return errs.Newf(errs.ClassHostFailure, "daemon.waitReady",
				"runtime stalled: no log growth or activity for %s (%s)%s",
				idle.Round(time.Second), act, o.runtimeSaid(ctx, sess))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.readyPoll()):
		}
	}
	return errs.Newf(errs.ClassHostFailure, "daemon.waitReady",
		"no completion before the deadline%s", o.runtimeSaid(ctx, sess))
}

// runtimeSaid collects the runtime's own account of what went wrong.
//
// The host is about to be destroyed, so anything not carried out in the error
// is lost — and the log is almost always where the answer is. A bring-up that
// fails with only LARRI's view of it ("no completion") tells the operator
// nothing they could act on.
func (o *Orchestrator) runtimeSaid(ctx context.Context, sess runtime.Session) string {
	// A fresh, short-lived context: the caller's may already be cancelled,
	// and this is the last chance to read anything at all.
	rctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = ctx

	rc, err := o.Runtime.Logs(rctx, sess, 25)
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 16<<10))
	if err != nil || len(b) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	// The last few non-empty lines: a Python traceback puts the cause last,
	// and a CUDA error puts it on the line that mentions CUDA.
	var keep []string
	for i := len(lines) - 1; i >= 0 && len(keep) < 6; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			keep = append([]string{l}, keep...)
		}
	}
	if len(keep) == 0 {
		return ""
	}
	out := "\n      runtime log:\n        " + strings.Join(keep, "\n        ")
	if len(out) > 1200 {
		out = out[:1200] + " …"
	}
	return out
}

func (o *Orchestrator) readyPoll() time.Duration {
	if o.ReadyPollInterval > 0 {
		return o.ReadyPollInterval
	}
	return 10 * time.Second
}

func (o *Orchestrator) readyCap() time.Duration {
	if o.ReadyCap > 0 {
		return o.ReadyCap
	}
	return 30 * time.Minute
}

// readLogState returns the runtime log's size and its last meaningful line.
//
// Size rather than mtime: a file touched but not written has not progressed,
// and growth is the only claim worth trusting.
func (o *Orchestrator) readLogState(ctx context.Context, sess runtime.Session) (int64, string) {
	path := o.runtimeLogPath()
	if path == "" {
		// A runtime that writes no log file leaves the hardware counters as
		// the only progress signal. They are weaker on their own, so this is
		// worth knowing rather than silently degrading into.
		return 0, ""
	}
	out, err := sess.Run(ctx, "stat -c %s "+path+" 2>/dev/null || echo 0; "+
		"tail -n 3 "+path+" 2>/dev/null | tr -d '\r' | grep -v '^$' | tail -n 1")
	if err != nil && len(out) == 0 {
		return 0, ""
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) == 0 {
		return 0, ""
	}
	size, _ := strconv.ParseInt(strings.TrimSpace(lines[0]), 10, 64)
	var tail string
	if len(lines) > 1 {
		tail = strings.TrimSpace(lines[len(lines)-1])
		if len(tail) > 160 {
			tail = tail[:160]
		}
	}
	return size, tail
}

// runtimeLogPath asks the engine where it writes, rather than assuming.
func (o *Orchestrator) runtimeLogPath() string {
	if lw, ok := o.Runtime.(runtime.LogWriter); ok {
		return lw.LogPath()
	}
	return ""
}

func humanSize(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
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

	// Bounded by progress rather than by a count.
	//
	// A fixed eight attempts was fine on the hardware it was written against
	// and wrong on the hardware LARRI exists to make usable. On a GTX 1050 Ti
	// pulling a CUDA image, sshd answers long before the provider's start-up
	// script has installed the rig key, and eighty seconds of retries ran out
	// while the image was still arriving. The host was working; LARRI gave up
	// on it and paid to start the same pull somewhere else.
	//
	// So the same rule as the endpoint wait (§12.2.1): while the provider
	// reports something new, the host is working and LARRI keeps trying. What
	// ends it is a stall.
	var (
		lastErr    error
		lastStatus string
		changedAt  = time.Now()
		deadline   = time.Now().Add(o.authCap())
	)
	for time.Now().Before(deadline) {
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
			switch {
			case isHostKeyMismatch(derr):
				o.emit("boot", "host key changed during boot; re-pinning")
			case isAuthFailure(derr):
				// The key LARRI generated is installed by the provider's
				// start-up script, which runs *after* sshd is listening. So
				// between the banner appearing and the script finishing there
				// is a window where the host is reachable and will not accept
				// us — and the endpoint probe, by getting us there sooner,
				// made that window easier to hit.
				//
				// During bring-up this is timing, not rejection. It stops
				// being timing once the attempts are exhausted, which is why
				// it is bounded rather than patient forever.
				o.emit("boot", "host not accepting the rig key yet; retrying")
			default:
				// A refused connection or a broken host is a real failure.
				return nil, nil, derr
			}
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(o.bootPoll()):
		}

		// Ask the provider whether anything is still happening. A host part
		// way through a 5 GB pull reports new layers every few seconds; a
		// host that has given up reports the same line forever.
		if cur, err := o.Provider.Get(ctx, inst.InstanceID); err == nil && cur != nil {
			if now := describeBoot(cur); now != lastStatus {
				if lastStatus != "" {
					o.emit("boot", "%s", now)
				}
				lastStatus, changedAt = now, time.Now()
			}
		}
		if idle := time.Since(changedAt); idle > o.authStall() {
			return nil, nil, errs.Newf(errs.ClassHostFailure, "daemon.pinAndDial",
				"host never accepted the rig key; nothing changed for %s: %v",
				idle.Round(time.Second), shortErr(lastErr))
		}
	}
	return nil, nil, errs.Newf(errs.ClassHostFailure, "daemon.pinAndDial",
		"host key never settled: %v", shortErr(lastErr))
}

// bootPoll is how often a booting host is re-examined.
func (o *Orchestrator) bootPoll() time.Duration {
	if o.BootPollInterval > 0 {
		return o.BootPollInterval
	}
	return 10 * time.Second
}

// authStall is how long the provider may report nothing new while the host
// still refuses the rig key. Zero means three minutes.
func (o *Orchestrator) authStall() time.Duration {
	if o.AuthStallTimeout > 0 {
		return o.AuthStallTimeout
	}
	return 3 * time.Minute
}

// authCap bounds the wait even on a host that keeps producing novel status.
func (o *Orchestrator) authCap() time.Duration {
	if o.AuthCap > 0 {
		return o.AuthCap
	}
	return 15 * time.Minute
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

// isAuthFailure reports a handshake rejected for credentials rather than for
// transport or identity.
func isAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	e := strings.ToLower(err.Error())
	return strings.Contains(e, "unable to authenticate") ||
		strings.Contains(e, "no supported methods remain") ||
		strings.Contains(e, "permission denied")
}

// idleSince renders how long a runtime has been quiet, rounded to something a
// person reads rather than parses.
func idleSince(t time.Time) time.Duration { return time.Since(t).Round(time.Second) }

// RuntimeLogs reads the runtime's log from the host of a serving rig.
//
// It needs the live SSH session rather than the rig snapshot, because the log
// lives on the rented machine and the snapshot is only what LARRI recorded
// about it. A caller holding a rig but not a Live cannot have this.
func (o *Orchestrator) RuntimeLogs(ctx context.Context, live *Live, tail int) (io.ReadCloser, error) {
	if live == nil || live.ssh == nil {
		return nil, errs.Newf(errs.ClassModelFailure, "daemon.RuntimeLogs",
			"rig has no ssh session")
	}
	return o.Runtime.Logs(ctx, live.ssh.Session(), tail)
}

// Activity exposes what the data plane has seen, for surfaces that display it.
// Nil when the rig has no proxy, which is the case before it serves.
func (l *Live) Activity() *wire.Activity {
	if l == nil || l.proxy == nil {
		return nil
	}
	return &l.proxy.Activity
}

// startBeating tells the host LARRI is still here, on its own goroutine.
//
// Separate from the supervisor because it has to run during bring-up too —
// the window before a rig serves is the longest one, and a LARRI that dies
// mid-pull is exactly what the watchdog exists for.
func (o *Orchestrator) startBeating(sess runtime.Session) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		t := time.NewTicker(deadman.BeatInterval)
		defer t.Stop()
		var missed int
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			bctx, bcancel := context.WithTimeout(ctx, 30*time.Second)
			err := deadman.Beat(bctx, sess)
			bcancel()
			if err == nil {
				missed = 0
				continue
			}
			// A missed beat is not an emergency: the deadline is many
			// intervals wide precisely so a blip has room to recover. It is
			// worth reporting once it stops looking like a blip.
			missed++
			if missed == 3 {
				o.warn("deadman", "%d heartbeats missed; the host will stop itself if this continues", missed)
			}
		}
	}()
	return cancel
}

// deadmanDeadline is how long the host waits before considering LARRI gone.
func (o *Orchestrator) deadmanDeadline() time.Duration {
	if o.DeadmanDeadline > 0 {
		return o.DeadmanDeadline
	}
	return deadman.Deadline(o.IdleTimeout)
}

// runtimePort is the loopback port the runtime serves on, so the watchdog can
// see requests in flight. Zero when the runtime has not said.
func (o *Orchestrator) runtimePort() int {
	if p, ok := o.Runtime.(interface{ Port() int }); ok {
		return p.Port()
	}
	return 8000
}
