// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"fmt"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/deadman"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/provider"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/sshx"
	"go.sovrenix.com/larri/internal/wire"
)

// Adopt rebuilds the data plane for a rig that outlived the process that
// created it (FR-SUP-07).
//
// There is no old tunnel to stop. A tunnel is an SSH connection and a local
// listener, both of which died with the process; the rented host merely saw a
// client disconnect. What survived is the part that costs money — the instance
// — and the part that is expensive to recreate — a runtime with the weights
// already resident in VRAM. Adopt reconnects to both.
//
// The obstacle is credentials, and Adopt solves each without ever having
// stored one:
//
//   - The SSH private key is gone, deliberately (FR-STATE-05). Rather than
//     recover it, Adopt mints a new pair and asks the provider to install the
//     public half on the running instance. The old key is not reused; it is
//     superseded, which is better hygiene than keeping it somewhere.
//   - The rig's API credential is read back from the running server's argv,
//     where Launch put it. It was never a secret from the host — the operator
//     has root — only from the network, and it stays one.
//
// No Hugging Face token is needed or accepted: adoption never downloads
// weights, so the credential that bring-up requires has no role here.
//
// Host key handling is deliberately stricter here than at bring-up. During a
// boot a changing host key is a host still settling, so it is re-pinned. After
// a rig has served, the key is known, and a key that no longer matches the
// recorded fingerprint means the endpoint is not the machine LARRI was talking
// to. Adopt refuses that outright instead of pinning whatever answers.
func (o *Orchestrator) Adopt(ctx context.Context, rigID string) (*Live, error) {
	rig, err := o.Store.Load(rigID)
	if err != nil {
		return nil, err
	}
	if rig.State == core.StateDestroyed {
		return nil, errs.Newf(errs.ClassModelFailure, "daemon.Adopt",
			"rig %s destroyed", rigID)
	}
	if rig.Instance == nil {
		return nil, errs.Newf(errs.ClassModelFailure, "daemon.Adopt",
			"rig %s never provisioned", rigID)
	}

	// ---- is it still there? ---------------------------------------------
	//
	// The provider is asked before anything is rebuilt, because reconnecting
	// to a rig that no longer exists should report that fact rather than time
	// out looking like a network problem.
	o.emit("adopt", "asking %s about instance %s", rig.Instance.Provider, rig.Instance.InstanceID)
	inst, err := o.Provider.Get(ctx, rig.Instance.InstanceID)
	if err != nil {
		return nil, err
	}
	if inst == nil {
		// Gone at the provider. Recording that closes the rig honestly and,
		// more to the point, stops the cost accountant from accruing against
		// a machine that is not billing.
		_ = o.Store.Transition(rig, core.StateDestroyed, "absent at provider on adopt")
		return nil, errs.Newf(errs.ClassModelFailure, "daemon.Adopt",
			"instance %s absent at provider", rig.Instance.InstanceID)
	}
	if !inst.Running {
		// STOPPED is billable for storage and is a decision, not a state to
		// paper over by silently restarting something the operator may want
		// destroyed (§12.4).
		_ = o.Store.Transition(rig, core.StateStopped, "found stopped on adopt")
		return nil, errs.Newf(errs.ClassModelFailure, "daemon.Adopt",
			"instance %s stopped, still billing storage: destroy or start it",
			rig.Instance.InstanceID)
	}
	rig.Instance = inst
	_ = o.Store.Save(rig)

	// ---- a new identity, installed through the provider ------------------
	attacher, ok := o.Provider.(provider.KeyAttacher)
	if !ok {
		return nil, errs.Newf(errs.ClassModelFailure, "daemon.Adopt",
			"%s cannot attach a key to a running instance: rig reachable for teardown only",
			rig.Instance.Provider)
	}
	keys, err := sshx.NewKeyPair()
	if err != nil {
		return nil, err
	}
	o.emit("adopt", "installing a fresh rig key %s", keys.Fingerprint())
	if err := attacher.AttachSSHKey(ctx, rig.Instance.InstanceID, keys.AuthorizedKey()); err != nil {
		return nil, err
	}

	live := &Live{Rig: rig, keys: keys}

	// ---- reconnect, verifying rather than pinning -------------------------
	client, err := o.dialPinned(ctx, inst, rig.HostKeyFingerprint, keys)
	if err != nil {
		return live, err
	}
	live.ssh = client
	sess := client.Session()
	o.emit("adopt", "reconnected, host key matches %s", rig.HostKeyFingerprint)

	// ---- find the server that is still running ---------------------------
	adopter, ok := o.Runtime.(runtime.Adopter)
	if !ok {
		return live, errs.Newf(errs.ClassModelFailure, "daemon.Adopt",
			"%s cannot re-attach to a running server", o.Runtime.Kind())
	}
	ep, err := adopter.Adopt(ctx, sess, rig.Model)
	if err != nil {
		return live, err
	}
	o.emit("adopt", "found %s serving %s on port %d", o.Runtime.Kind(), ep.Model, ep.Port)

	// Re-arm, because the watchdog this rig was left with has been counting
	// the whole time LARRI was gone and may be moments from acting. Arming
	// resets its clock and replaces it, which is the same operation.
	if o.DeadmanDeadline >= 0 {
		cfg := deadman.Config{
			Deadline:    o.deadmanDeadline(),
			RuntimePort: ep.Port,
			RuntimeLog:  o.runtimeLogPath(),
		}
		if err := deadman.Arm(ctx, sess, cfg); err != nil {
			o.warn("deadman", "could not re-arm the host watchdog: %s", shortErr(err))
		} else {
			o.emit("deadman", "host watchdog re-armed (%s)", cfg.Deadline.Round(time.Minute))
			live.beating = o.startBeating(sess)
		}
	}

	// ---- rebuild the tunnel on the port clients already point at ---------
	//
	// Reusing rig.LocalPort is the entire point of recovery. Clients were
	// wired to that address and have not been told anything changed; handing
	// them a different port would make a successful adopt indistinguishable
	// from a failed one at every tool that matters.
	if err := o.attachTunnel(ctx, live, rig, ep, rig.LocalPort); err != nil {
		return live, err
	}
	o.emit("tunnel", "%s → %s:%d (restored)", live.Endpoint, inst.SSHHost, ep.Port)

	// ---- prove it, the same way a bring-up proves it ---------------------
	o.emit("ready", "waiting for a completion to round-trip")
	if err := o.waitReady(ctx, sess, rig, live.proxy.LocalPort(), live.ClientToken); err != nil {
		return live, err
	}
	if err := o.Store.Transition(rig, core.StateReady, "adopted after restart"); err != nil {
		return live, err
	}
	return live, nil
}

// dialPinned connects only to the host whose key matches want.
//
// A mismatch here is not a retry case. During a boot the key is still being
// generated, so a change means "not settled"; after a rig has served, the key
// is a fact about the machine, and a different one means the endpoint now
// leads somewhere else. That is the scenario host-key pinning exists to catch,
// and treating it as transient would discard the only evidence of it.
//
// Authentication failure *is* a retry case, briefly: the provider installs the
// new key asynchronously, so a handshake can arrive before authorized_keys
// does.
func (o *Orchestrator) dialPinned(ctx context.Context, inst *core.Instance,
	want string, keys *sshx.KeyPair) (*sshx.Client, error) {

	const attempts = 6
	var lastErr error
	for i := 0; i < attempts; i++ {
		hostKey, err := sshx.ScanHostKey(ctx, inst.SSHHost, inst.SSHPort, 45*time.Second)
		if err != nil {
			lastErr = err
		} else {
			if got := sshx.Fingerprint(hostKey); want != "" && got != want {
				return nil, errs.Newf(errs.ClassSecurity, "daemon.dialPinned",
					"host key changed: expected %s, got %s", want, got)
			}
			client, derr := sshx.Dial(ctx, sshx.Config{
				Host: inst.SSHHost, Port: inst.SSHPort, User: "root",
				Key: keys, HostKey: hostKey, Timeout: 60 * time.Second,
			})
			if derr == nil {
				return client, nil
			}
			lastErr = derr
			if !isAuthFailure(derr) {
				return nil, derr
			}
			o.emit("adopt", "key not installed yet; retrying")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return nil, errs.Newf(errs.ClassHostFailure, "daemon.dialPinned",
		"reconnect failed: %v", shortErr(lastErr))
}

// attachTunnel opens the forward and proxy for an endpoint and records them on
// live. Serve and Adopt share it so a restored rig is wired exactly like a
// fresh one — including the credential substitution, which is what keeps a
// client token off the rented host (FR-SEC-22).
func (o *Orchestrator) attachTunnel(ctx context.Context, live *Live, rig *core.Rig,
	ep runtime.Endpoint, localPort int) error {

	fwd, err := live.ssh.Listen(0, ep.Port)
	if err != nil {
		return err
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
			return err
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
		return err
	}
	proxy.AddClient("larri-cli", token)
	live.ClientToken = token
	rig.LocalPort = proxy.LocalPort()
	live.Endpoint = fmt.Sprintf("http://127.0.0.1:%d/v1", rig.LocalPort)
	return nil
}
