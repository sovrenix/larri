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
	"go.sovrenix.com/larri/internal/provider"
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

	// The weights come from the internet, so the host has to be able to
	// reach it. Asked here because the alternative is discovering it the
	// expensive way: a live run rented a host whose egress was broken,
	// launched vLLM, and watched it retry Hugging Face five times before
	// dying with "We couldn't connect to https://huggingface.co". The
	// evidence was on screen throughout as "net 0.0 MB/s" and nobody was
	// reading it.
	if err := o.verifyEgress(ctx, sess, rig); err != nil {
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
		lastEndpoint  core.Instance
		lastLogLine   string
		lastSeen      string
		lastSig       string
		changedAt     = time.Now()
		deadline      = time.Now().Add(cap)
		everSpoke     bool
		endpointAt    time.Time
		announcedEP   bool
		announcedPull bool
		offlineSince  time.Time
	)
	for time.Now().Before(deadline) {
		inst, err := o.Provider.Get(ctx, rig.Instance.InstanceID)
		// Watched before the dispatch rather than as a case of it: a host
		// that flaps offline for one poll and carries on must keep its
		// place, so this records when the outage began and clears it the
		// moment the host answers again.
		switch {
		case inst == nil || !bootOffline(inst):
			offlineSince = time.Time{}
		case offlineSince.IsZero():
			offlineSince = time.Now()
			o.warn("boot", "host reports offline; giving it %s to come back", offlineGrace)
		}
		switch {
		case err != nil:
			// Unreachable is not absent; keep asking (FR-SUP-11). A provider
			// outage must not be mistaken for a dead host.
			o.warn("boot", "status query failed: %v", err)
		case inst == nil:
			return nil, errs.Newf(errs.ClassProviderUnknownOutcome, "daemon.waitForSSH",
				"instance %s vanished during boot", rig.Instance.InstanceID)

		case bootOffline(inst) && !offlineSince.IsZero() && time.Since(offlineSince) > offlineGrace:
			// The machine has dropped off the provider's network and stayed
			// off. Nothing LARRI waits for can happen while it is gone.
			return nil, errs.Newf(errs.ClassHostFailure, "daemon.waitForSSH",
				"host went offline and stayed offline for %s",
				time.Since(offlineSince).Round(time.Second))

		case bootAbandoned(inst):
			// The provider has stopped trying. Every other clock in this loop
			// measures patience, and patience is the wrong instrument once
			// the host says it is giving up.
			return nil, errs.Newf(errs.ClassHostFailure, "daemon.waitForSSH",
				"provider gave up on the instance: %s", bootFailureReason(inst))

		case inst.SSHHost != "" && inst.SSHPort > 0 || lastEndpoint.SSHHost != "":
			// An address, once published, is remembered.
			//
			// RunPod's pod reads drop publicIp and portMappings intermittently
			// — measured over ten minutes, the same pod alternated between
			// reporting its address and reporting none, several times. Taking
			// each absence at face value made LARRI forget an endpoint it had
			// already been given and restart the wait, so the probe never got
			// a run at the address long enough to matter.
			//
			// Remembering is safe because an endpoint is only ever *tested*,
			// never trusted: if the address is stale the probe fails and the
			// stall limit ends it exactly as before.
			if inst.SSHHost != "" && inst.SSHPort > 0 {
				lastEndpoint = *inst
			} else {
				inst.SSHHost, inst.SSHPort = lastEndpoint.SSHHost, lastEndpoint.SSHPort
			}
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
			} else if waited := time.Since(endpointAt); waited > unreachable && !bootPending(inst) {
				// Measured from the last sign of life, not from publication.
				// See the reset below.
				return nil, errs.Newf(errs.ClassHostFailure, "daemon.waitForSSH",
					"endpoint %s:%d unreachable for %s while nothing changed: %v",
					inst.SSHHost, inst.SSHPort, waited.Round(time.Second), shortErr(perr))
			} else if bootPending(inst) && !announcedPull {
				announcedPull = true
				o.emit("boot", "%s — image still arriving; not counting against the reachability window", inst.Status)
			}
			fallthrough

		default:
			if inst != nil && inst.StatusMsg != "" {
				everSpoke = true
			}
			if inst != nil {
				// A provider that narrates its boot in logs rather than in a
				// status field still gets a progress-driven wait. RunPod's
				// desiredStatus is RUNNING from the first second and never
				// changes; its log says "Pulling from", "Status: Image is up
				// to date", "start container: begin". Same signal, different
				// source — and without it the wait falls back to a fixed
				// clock, which is what §12.2.1 exists to avoid.
				if line, ok := o.latestBootLog(ctx, rig); ok && line != lastLogLine {
					lastLogLine = line
					changedAt = time.Now()
					if !endpointAt.IsZero() {
						endpointAt = time.Now()
					}
					o.emit("boot", "%s", line)
				}
				if sig := bootSignature(inst); sig != lastSig {
					now := describeBoot(inst)
					o.emit("boot", "%s", now)
					lastSig, lastSeen, changedAt = sig, now, time.Now()
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
			// A container the provider is still preparing gets longer.
			//
			// Docker narrates a download and then goes quiet to unpack it,
			// and the vLLM image has a single 4.6 GB layer whose extraction
			// is disk-bound and silent. A live run died at eight minutes on
			// "Pull complete" — the host was unpacking, not stuck. The
			// tolerance is safe to widen because the dangerous case no longer
			// depends on it: a provider that gives up is now detected from
			// its own intent, and a machine that drops off is detected from
			// its status, both in seconds rather than minutes.
			limit := stall
			if bootPending(inst) {
				limit = stall * 2
			}
			if idle := time.Since(changedAt); idle > limit {
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
// bootPending reports whether the provider says the container has not been
// started yet, so an address that refuses connections is expected rather than
// suspicious.
//
// This is the distinction the reachability window kept getting wrong. Vast
// publishes an SSH endpoint when the *contract* starts, which is before the
// image has been pulled, so the window opened while sshd could not exist yet
// and closed three minutes later on a host that was working the whole time. A
// stock vLLM image is 10-15 GB and the cheap multi-GPU boxes that make cheap
// inference possible are exactly the ones that pull it slowly — a live run
// burned three rentals this way, each abandoned mid-download and restarted
// from nothing on a fresh machine.
//
// Only an explicitly reported *container* phase counts. "contract running"
// with actual_status never set is the opposite case — the contract is billing
// and the container has not been reported at all — and a live run watched one
// sit that way for ten minutes against an address nothing was listening on.
// That is the failure the window exists to catch, so its clock runs normally.
//
// A host that sits in "loading" forever is caught too, by the stall timeout,
// which is the honest instrument for it: no status change for eight minutes is
// what "stopped trying" means.
func bootPending(inst *core.Instance) bool {
	switch strings.ToLower(strings.TrimSpace(inst.Status)) {
	case "loading", "created", "creating", "pulling", "starting", "scheduling":
		return true
	}
	return false
}

// offlineGrace is how long a machine may report offline before LARRI treats
// it as gone.
//
// Not zero, because the status flaps: a live run saw a host report offline
// for one poll in the middle of an image pull and carry straight on. Acting
// on the first sample would destroy a working rental; ignoring the status
// entirely costs five minutes of the reachability clock on a host that has
// actually vanished.
const offlineGrace = 90 * time.Second

// bootOffline reports whether the provider says the machine itself is
// unreachable, as distinct from the container not being ready.
func bootOffline(inst *core.Instance) bool {
	return strings.EqualFold(strings.TrimSpace(inst.Status), "offline")
}

// bootAbandoned reports whether the provider has stopped trying to start the
// instance.
//
// This is the distinction every timeout in this loop is blind to. A live run
// watched an instance report actual_status "created" — indistinguishable from
// a boot in progress, and treated as one — while intended_status had already
// flipped to "stopped" because the container could not be created at all:
//
//	OCI runtime create failed: ... failed to inject CDI devices:
//	unresolvable CDI devices
//
// The GPUs could not be attached, so that container was never going to run.
// LARRI printed "image still arriving" at it and waited out the stall timeout
// for nothing, while the operator watched the provider's own console show
// Inactive and an error. What the host intends is the only signal that
// resolves this, and it is decisive: no amount of waiting reverses it.
func bootAbandoned(inst *core.Instance) bool {
	switch strings.ToLower(strings.TrimSpace(inst.Intent)) {
	case "stopped", "exited", "error", "failed":
		return true
	}
	return false
}

// bootFailureReason prefers the provider's own message, which usually names
// the cause precisely, and falls back to the status pair.
func bootFailureReason(inst *core.Instance) string {
	if msg := strings.TrimSpace(inst.StatusMsg); msg != "" {
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return msg
	}
	return fmt.Sprintf("status %s, intent %s", orUnknown(inst.Status), orUnknown(inst.Intent))
}

// bootSignature is what the stall detector compares, and it deliberately uses
// the *whole* status message rather than the line shown to the operator.
//
// A live run died on this three times. Vast's status_msg is a rolling buffer
// of docker's pull output, and new progress is appended to the end — but the
// comparison used describeBoot, which truncates to the first 120 characters
// for display. So while a 15 GB image was actively downloading, the prefix sat
// unchanged, the stall timer read it as silence, and the host was destroyed at
// eight minutes with its pull most of the way done.
//
// Display truncates; progress detection must not.
func bootSignature(inst *core.Instance) string {
	return inst.Status + "\x00" + inst.StatusMsg + "\x00" + strconv.FormatFloat(inst.DiskUsedGB, 'f', 2, 64)
}

func describeBoot(inst *core.Instance) string {
	status := inst.Status
	if status == "" {
		status = "starting"
	}
	if msg := lastLine(inst.StatusMsg); msg != "" {
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

// lastLine returns the newest line of a provider's status buffer.
//
// The buffer accumulates, so its first line is the oldest news in it. Showing
// the head means reporting what the host was doing several minutes ago while
// claiming it is current.
func lastLine(msg string) string {
	msg = strings.TrimRight(msg, "\n\r \t")
	if i := strings.LastIndexAny(msg, "\n\r"); i >= 0 {
		return strings.TrimSpace(msg[i+1:])
	}
	return strings.TrimSpace(msg)
}

func orUnknown(s string) string {
	if s == "" {
		return "no status reported"
	}
	return s
}

// readComputeCapability asks the placed GPU what architecture it is.
//
// nvidia-smi reports it as "7.0"; it is scaled by 100 to match the integer
// the rest of LARRI compares against. Zero means the question went
// unanswered, and the caller falls back to the listing.
func (o *Orchestrator) readComputeCapability(ctx context.Context, sess runtime.Session) int {
	out, err := sess.Run(ctx,
		"nvidia-smi --query-gpu=compute_cap --format=csv,noheader 2>/dev/null || true")
	if err != nil {
		return 0
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]), 64)
	if err != nil || f <= 0 {
		return 0
	}
	return int(f*100 + 0.5)
}

// KnownHFMirrors are Hugging Face-compatible hosts to offer when the real one
// cannot be reached.
//
// Named, not used. A mirror serves the weights that end up in the model's
// memory, so choosing one is a supply-chain decision and belongs to the
// operator; LARRI's part is to say which ones this host can actually reach,
// because a suggestion that also fails to resolve wastes another rental.
var KnownHFMirrors = []string{"https://hf-mirror.com"}

// egressControl is the host used to tell "this machine has no internet" from
// "this machine cannot reach Hugging Face".
//
// Docker Hub, because the machine has demonstrably just used it: the runtime
// image arrived over it minutes earlier, so a failure here means the network
// broke rather than that the host was never able to route anywhere.
const egressControl = "https://registry-1.docker.io/v2/"

// verifyEgress confirms the host can reach where the weights live.
//
// One round of requests, before anything is downloaded. A host that cannot
// reach the weight source fails after the image, the launch and several
// minutes of retries, and every second of that is billed — so the question is
// asked while the answer is still cheap. A live run proved the case twice
// over: the same GTX 1660 S that had burned four minutes reaching for
// huggingface.co was rejected here in twenty seconds.
//
// The control request is what makes the verdict useful rather than merely
// correct. A machine with no route anywhere is a bad rental and should be
// abandoned; a machine that simply cannot reach Hugging Face is a good rental
// in a region that cannot route to it, and the remedy is a mirror rather than
// a different host.
//
// Advisory in one direction only: an unreachable source with a working
// control is a hard failure, but a probe that cannot run at all (no curl, no
// python) is not evidence of anything and lets the launch proceed.
func (o *Orchestrator) verifyEgress(ctx context.Context, sess runtime.Session, rig *core.Rig) error {
	host := weightsHost(rig.Model)
	if host == "" {
		return nil
	}
	targets := []string{"https://" + host, egressControl}
	mirrors := []string{}
	if rig.Model.Source == core.SourceHuggingFace {
		mirrors = KnownHFMirrors
		targets = append(targets, mirrors...)
	}

	ectx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	reach := o.probeReach(ectx, sess, targets)
	if reach == nil {
		return nil // the probe itself could not run; not evidence
	}
	if reach["https://"+host] {
		o.emit("boot", "host reaches %s", host)
		return nil
	}
	if reach[egressControl] {
		o.warn("egress", "this host cannot reach %s but is otherwise online — common where the region cannot route to it", host)
		if usable := reachableAmong(mirrors, reach); len(usable) > 0 {
			o.warn("egress", "reachable mirror(s): %s — weights would come from a third party, so it is opt-in",
				strings.Join(usable, ", "))
		}
	}
	return egressVerdict(host, mirrors, reach)
}

// reachableAmong filters candidates to the ones this host answered for.
func reachableAmong(candidates []string, reach map[string]bool) []string {
	var out []string
	for _, c := range candidates {
		if reach[c] {
			out = append(out, c)
		}
	}
	return out
}

// egressVerdict turns a set of probe results into a decision.
//
// Separated from the probing so the judgement can be tested without a host,
// which is the half that has to be right: the difference between "abandon
// this rental" and "this rental is fine, point it at a mirror".
func egressVerdict(host string, mirrors []string, reach map[string]bool) error {
	if reach["https://"+host] {
		return nil
	}
	if !reach[egressControl] {
		return errs.Newf(errs.ClassHostFailure, "daemon.verifyEgress",
			"host has no working internet: neither %s nor docker.io responded", host)
	}
	usable := reachableAmong(mirrors, reach)
	if len(usable) == 0 {
		return errs.Newf(errs.ClassHostFailure, "daemon.verifyEgress",
			"host cannot reach %s, and no known mirror either", host)
	}
	return errs.Newf(errs.ClassHostFailure, "daemon.verifyEgress",
		"host cannot reach %s: retry with --hf-endpoint %s", host, usable[0])
}

// probeReach asks the host which of these URLs answer, in one command.
//
// Returns nil when the probe could not run, which is different from a map of
// failures: the first means LARRI learned nothing, the second means the host
// answered and the answer was no.
func (o *Orchestrator) probeReach(ctx context.Context, sess runtime.Session, urls []string) map[string]bool {
	var b strings.Builder
	b.WriteString("if command -v curl >/dev/null 2>&1; then P=curl; " +
		"elif command -v python3 >/dev/null 2>&1; then P=py; else echo LARRI_SKIP; exit 0; fi\n")
	for _, u := range urls {
		fmt.Fprintf(&b, "if [ \"$P\" = curl ]; then "+
			"curl -sS -o /dev/null -m 15 %s >/dev/null 2>&1 && echo 'OK %s' || echo 'NO %s'; "+
			"else python3 -c \"import urllib.request as r;r.urlopen('%s',timeout=15)\" >/dev/null 2>&1 "+
			"&& echo 'OK %s' || echo 'NO %s'; fi\n",
			shellQuoteURL(u), u, u, u, u, u)
	}
	out, err := sess.Run(ctx, b.String())
	text := string(out)
	if strings.Contains(text, "LARRI_SKIP") {
		return nil
	}
	if err != nil && strings.TrimSpace(text) == "" {
		return nil
	}
	reach := map[string]bool{}
	seen := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if u, ok := strings.CutPrefix(line, "OK "); ok {
			reach[u], seen = true, true
		} else if u, ok := strings.CutPrefix(line, "NO "); ok {
			if _, exists := reach[u]; !exists {
				reach[u] = false
			}
			seen = true
		}
	}
	if !seen {
		return nil
	}
	return reach
}

// shellQuoteURL wraps a URL for the shell. URLs LARRI builds are not operator
// input, but they are interpolated into a script and quoting costs nothing.
func shellQuoteURL(u string) string {
	return "'" + strings.ReplaceAll(u, "'", `'\''`) + "'"
}

// weightsHost is where this model's weights are fetched from.
func weightsHost(spec core.ModelSpec) string {
	switch spec.Source {
	case core.SourceOllamaRegistry:
		return "registry.ollama.ai"
	default:
		return "huggingface.co"
	}
}

// verifyPlacedHardware re-runs the fit check against the machine that was
// actually provisioned.
func (o *Orchestrator) verifyPlacedHardware(ctx context.Context, sess runtime.Session, rig *core.Rig) error {
	if cap := o.readComputeCapability(ctx, sess); cap > 0 {
		rig.Plan.ComputeCapability = cap
	} else if c := rig.Offer.ComputeCapability; c > 0 {
		rig.Plan.ComputeCapability = c // the listing, when the host will not say
	}
	out, err := sess.Run(ctx,
		"nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits 2>/dev/null || true")
	if err != nil || len(out) == 0 {
		// Telemetry is subordinate (T1): failing to read the GPU must not
		// fail a rig. The sizing plan already passed against the offer.
		o.warn("boot", "could not read GPU memory to re-verify sizing")
		return nil
	}
	totalMB, gpus := sumGPUMemoryMB(string(out))
	if totalMB <= 0 {
		return nil
	}
	haveBytes := uint64(totalMB) * 1024 * 1024
	if haveBytes < rig.Plan.RequiredVRAMBytes {
		return errs.Newf(errs.ClassHostFailure, "daemon.verifyPlacedHardware",
			"placed hardware has %s across %d gpu(s) but the plan needs %s",
			sizing.HumanBytes(haveBytes), gpus, sizing.HumanBytes(rig.Plan.RequiredVRAMBytes))
	}
	// A box with fewer cards than the listing promised still has to shard the
	// model across what is actually there, so the launch plan is corrected to
	// the hardware rather than to the advertisement.
	if gpus > 0 && gpus != rig.Plan.TensorParallelSize {
		o.warn("boot", "listing promised %d gpu(s), host has %d — sharding across %d",
			rig.Plan.TensorParallelSize, gpus, gpus)
		rig.Plan.TensorParallelSize = gpus
	}
	o.emit("boot", "GPU reports %s across %d gpu(s), plan needs %s",
		sizing.HumanBytes(haveBytes), gpus, sizing.HumanBytes(rig.Plan.RequiredVRAMBytes))
	return nil
}

// sumGPUMemoryMB totals the VRAM nvidia-smi reports, and counts the cards.
//
// The query prints one line per GPU. Reading only the first compares a single
// card against a requirement the whole box is meant to satisfy, which rejects
// exactly the multi-GPU hosts that are the only affordable way to hold a large
// model: a live run rented a 3x3090 box and failed it for "24.0 GB but the
// plan needs 64.0 GB", then an 8x3060 box for "12.0 GB", each after paying to
// boot and pull the image.
//
// Summing is what the plan already assumes — sizing multiplies VRAM per GPU by
// GPU count, and tensor parallelism shards the weights across every card.
func sumGPUMemoryMB(out string) (totalMB, gpus int) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if mb := parseFirstInt(line); mb > 0 {
			totalMB += mb
			gpus++
		}
	}
	return totalMB, gpus
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
		fetch      fetchProgress
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
				o.lastProgress = tail
				o.emit("ready", "%s", tail)
			}
		} else if act.Moving {
			// The log can go quiet mid-phase while the machine is plainly
			// working: loading a checkpoint into VRAM writes nothing.
			lastGrowth = time.Now()
		}
		if attempts%4 == 0 {
			if p := fetch.sample(ctx, o, sess, rig.Plan.WeightsBytes); p != "" {
				o.emit("ready", "%s", p)
			} else {
				o.emit("ready", "log %s · %s", humanSize(logBytes), act)
			}
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
					said := o.runtimeSaid(ctx, sess)
					return errs.Newf(errs.ClassHostFailure, "daemon.waitReady",
						"runtime exited without serving%s; log quiet for %s%s",
						because(said), idleSince(lastGrowth), said)
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

	// "See root cause above" is a real vLLM message, and the cause can be
	// dozens of lines above the failure — a traceback, then a shutdown
	// sequence printed after it. Twenty-five lines routinely contains only
	// the shutdown.
	rc, err := o.Runtime.Logs(rctx, sess, 250)
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 16<<10))
	if err != nil || len(b) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	keep := diagnosticLines(lines)
	if len(keep) == 0 {
		return ""
	}
	out := "\n      runtime log:\n        " + strings.Join(keep, "\n        ")
	if len(out) > 3000 {
		out = out[:3000] + " …"
	}
	return out
}

// because lifts the one line most likely to be the cause into the summary.
//
// The full log is attached below the error and an operator will read it, but
// the summary is what appears in a fallback's one-line report, in a journal
// entry, and in whatever an agent driving MCP decides to surface. A run that
// said only "runtime exited without serving" three times over hid that all
// three were the same cause.
func because(said string) string {
	best := ""
	for _, l := range strings.Split(said, "\n") {
		l = strings.TrimSpace(l)
		// Python names the exception on a line of the form "OSError: …";
		// CUDA and the loaders announce themselves the same way.
		if i := strings.Index(l, "Error: "); i >= 0 && len(l) > i+7 {
			best = strings.TrimSpace(l[strings.LastIndex(l[:i+1], " ")+1:])
		}
	}
	if best == "" {
		return ""
	}
	if len(best) > 140 {
		best = best[:140] + "…"
	}
	return ": " + best
}

// fetchProgress turns successive measurements of the weights cache into the
// answer an operator actually wants.
//
// The throughput figure alone does not give it. "net 14 MB/s" says something
// is moving; it does not say whether that is two minutes from done or forty,
// and that difference is what decides between waiting and destroying. Bytes
// against expected bytes, with a rate measured over the gap, does.
type fetchProgress struct {
	last     uint64
	lastAt   time.Time
	rate     float64 // bytes/sec, smoothed
	finished bool
}

// sample measures once and renders a line, or returns "" when there is
// nothing worth saying.
//
// Silent unless it can be useful: a runtime that cannot measure, a plan with
// no expected size, and a download that has already finished all produce
// nothing, and the caller falls back to its ordinary reporting.
func (f *fetchProgress) sample(ctx context.Context, o *Orchestrator,
	sess runtime.Session, want uint64) string {

	if f.finished || want == 0 {
		return ""
	}
	wp, ok := o.Runtime.(runtime.WeightsProgressor)
	if !ok {
		return ""
	}
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	got, err := wp.WeightsOnDisk(sctx, sess)
	if err != nil || got == 0 {
		return ""
	}
	now := time.Now()
	if !f.lastAt.IsZero() && got > f.last {
		if dt := now.Sub(f.lastAt).Seconds(); dt > 0 {
			r := float64(got-f.last) / dt
			// Smoothed, because a du that lands mid-write reads low and a
			// burst reads high, and an estimate that swings between four
			// minutes and forty is worse than none.
			if f.rate == 0 {
				f.rate = r
			} else {
				f.rate = 0.6*f.rate + 0.4*r
			}
		}
	}
	f.last, f.lastAt = got, now

	// Past the expected size means the estimate was low, not that something
	// is wrong — the cache holds tokeniser and config files too. Report it
	// as done rather than as 103%.
	if got >= want {
		f.finished = true
		return fmt.Sprintf("weights %s fetched — loading into VRAM", sizing.HumanBytes(got))
	}
	pct := 100 * float64(got) / float64(want)
	line := fmt.Sprintf("weights %s of %s (%.0f%%)",
		sizing.HumanBytes(got), sizing.HumanBytes(want), pct)
	if f.rate > 0 {
		remain := time.Duration(float64(want-got)/f.rate) * time.Second
		line += fmt.Sprintf(" · %.0f MB/s · ~%s left", f.rate/1e6, remain.Round(time.Second))
	}
	return line
}

// errorSignatures are what a runtime's own account of a failure looks like.
var errorSignatures = []string{
	"Traceback", "Error", "error:", "Exception", "CUDA", "assert",
	"Failed", "FAILED", "No module", "not supported", "unsupported",
	"out of memory", "Killed", "Aborted",
}

// diagnosticLines picks the part of a log that explains a failure.
//
// Taking the last N lines is the obvious rule and the wrong one: engines
// print a shutdown sequence *after* the thing that killed them, so the tail
// is the tidy-up and the cause has already scrolled past. A live run
// surfaced "next(self.gen)" — a frame from the middle of a traceback — while
// "Engine core initialization failed" sat above it, unread.
//
// So the window is anchored on the last line that looks like a cause, with
// the lines around it for context, and falls back to the tail only when
// nothing in the log looks like an error at all.
func diagnosticLines(lines []string) []string {
	anchor := -1
	for i := len(lines) - 1; i >= 0; i-- {
		for _, sig := range errorSignatures {
			if strings.Contains(lines[i], sig) {
				anchor = i
				break
			}
		}
		if anchor >= 0 {
			break
		}
	}
	const before, after = 8, 4
	lo, hi := 0, len(lines)
	if anchor >= 0 {
		if anchor-before > 0 {
			lo = anchor - before
		}
		if anchor+after+1 < hi {
			hi = anchor + after + 1
		}
	} else if hi-6 > 0 {
		lo = hi - 6
	}
	var keep []string
	for _, l := range lines[lo:hi] {
		if t := strings.TrimSpace(l); t != "" {
			keep = append(keep, t)
		}
	}
	return keep
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
	// Truncate the summary, never the evidence. A failing runtime's own log
	// is attached to the error after a newline, and capping the whole string
	// at 160 characters threw it away mid-word: a live run reported
	// "(APIServer pid=435) next(self.gen)" and then "(APIS", which is the
	// middle of a Python traceback and diagnoses nothing.
	head, rest, multi := strings.Cut(s, "\n")
	if len(head) > 160 {
		head = head[:160]
	}
	if !multi {
		return head
	}
	if len(rest) > 4000 {
		rest = rest[:4000] + " …"
	}
	return head + "\n" + rest
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

// latestBootLog returns the newest line a provider has to offer about a
// booting instance, if it offers any.
//
// Only the last line, deliberately. This drives a *progress* decision — has
// anything happened since we last looked — and for that the tail is the whole
// answer. Streaming the lot into the event feed would bury the phases an
// operator is watching for under docker layer noise.
func (o *Orchestrator) latestBootLog(ctx context.Context, rig *core.Rig) (string, bool) {
	if rig.Instance == nil {
		return "", false
	}
	lctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	lines, ok := provider.BootLogOf(lctx, o.Provider, rig.Instance.InstanceID, 20)
	if !ok || len(lines) == 0 {
		return "", false
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	if len(last) > 160 {
		last = last[:160]
	}
	return last, last != ""
}
