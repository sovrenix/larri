// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package deadman arms a rented host to stop costing money if LARRI goes away.
//
// LARRI's idle reclamation and budget ceilings run in the local process, so
// they stop being enforced the moment that process does — a killed terminal, a
// closed laptop, a lost network. The instance keeps billing and the operator
// finds out on an invoice. A guarantee that depends on someone's laptop
// staying awake is not much of a guarantee.
//
// # What the host is allowed to do
//
// The obvious design is to let the host destroy itself through the provider
// API, and it is not available here. That would mean writing an
// account-scoped key onto a machine whose operator is not you and has root —
// a key that can destroy every instance on the account, not just this one. No
// idle-timeout guarantee is worth that trade (§15.4).
//
// So the watchdog uses only powers the host already holds over itself: it
// stops the runtime, then attempts to halt the container.
//
// # A Vast container cannot stop its own billing
//
// This was measured rather than assumed. A probe rented an instance and tried
// every method in turn, giving the provider two minutes to notice each:
//
//	pid1 = bash /.launch        dockerenv = yes    (own pid namespace)
//	CapBnd = 00000000a80405fb   → CAP_SYS_BOOT clear
//
//	halt -f       → "Failed to halt: Operation not permitted"   still running
//	poweroff -f   → refused                                     still running
//	kill -TERM 1  → rc=0, signal delivered                      still running
//	kill -KILL 1  → rc=0                                        still running
//
// The capability bound explains the first two: without CAP_SYS_BOOT the kernel
// refuses, and no amount of privilege inside the container changes that.
// Signalling PID 1 is permitted and does nothing useful — Vast's launcher is
// restarted, and the rental is billed for regardless of what runs inside it.
//
// So the halt is attempted and expected to fail here. It stays because it is
// provider-neutral and another provider may permit it; it is logged as an
// attempt, never as an outcome.
//
// # What this actually buys: containment, not cost
//
// Stopping the runtime works, and is worth having on its own. An abandoned rig
// stops answering prompts and releases VRAM — the model is no longer serving
// anyone, including whoever else might reach it. That is a security property,
// not a financial one.
//
// **Ending the bill is not achievable from inside a marketplace container.**
// It needs a provider call from the operator's machine: `larri down`, or
// `larri orphans` for a rig nothing is tracking. Every message this package
// produces says exactly that and no more, and a test fails if one promises
// otherwise.
//
// # Two signals, never one
//
// A watchdog that halted purely on elapsed time would be wrong in the
// expensive direction. A missed heartbeat says LARRI is gone; it says nothing
// about whether the *host* is doing something worth keeping — a 40 GB weight
// download, a checkpoint loading into VRAM, a generation still running for a
// request whose tunnel dropped. Halting through any of those throws away
// precisely the work a returning operator would want to resume, and they paid
// for it.
//
// So the rule is the one §12.2.1 already settled for readiness: **act on
// silence, not on the clock.** The deadline only opens the question; the host
// then has to be quiet as well — no GPU work, no connections to the runtime,
// no log growth, no disk or network movement. A busy host keeps its grace,
// up to a cap, because a host that looks busy forever is still a host nobody
// is using.
//
// # What it does not defend against
//
// A hostile host operator. They have root; they can kill the watchdog, lie
// about the clock, or simply keep billing. This exists to survive LARRI dying,
// not to survive the machine's owner — and nothing running on their hardware
// could do the latter.
package deadman

import (
	"context"
	"fmt"
	"time"

	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
)

const (
	// Dir holds the watchdog's files, under /var/run so a reboot clears them.
	Dir = "/var/run/larri"
	// beatPath is touched by LARRI; its mtime is the entire protocol.
	beatPath = Dir + "/heartbeat"
	// scriptPath is the watchdog itself.
	scriptPath = Dir + "/watchdog.sh"
	// LogPath records what the watchdog saw and did, so a halted rig can
	// explain itself when the operator comes back to it.
	LogPath = "/var/log/larri-watchdog.log"

	// BeatInterval is how often LARRI checks in — frequent enough that a
	// transient failure has many chances to recover before the deadline.
	BeatInterval = 60 * time.Second

	// MinDeadline floors how long a host waits before even asking whether it
	// is idle.
	//
	// The watchdog is a backstop, not a competitor: the local supervisor can
	// tell a busy rig from an idle one and must always act first. A deadline
	// short enough to race it would halt rigs that were merely mid-request.
	MinDeadline = 15 * time.Minute

	// MaxGrace bounds how long a *busy-looking* host may extend itself past
	// the deadline. Without it, a machine with a stuck process pegging a core
	// would bill forever on the strength of looking busy.
	MaxGrace = 2 * time.Hour
)

// Deadline returns how long the host waits before considering itself
// abandoned, given the local idle policy.
//
// Comfortably longer than the local timeout, so in normal operation the
// supervisor has already acted and the watchdog never fires at all. Firing
// means LARRI is gone.
func Deadline(idle time.Duration) time.Duration {
	d := 2 * idle
	if idle == 0 {
		d = 30 * time.Minute
	}
	if d < MinDeadline {
		d = MinDeadline
	}
	return d
}

// Config is what the host is armed with.
type Config struct {
	// Deadline is the silence after which the host asks whether it is idle.
	Deadline time.Duration
	// RuntimePort is the loopback port the runtime serves on, so the watchdog
	// can see requests in flight.
	RuntimePort int
	// RuntimeLog is the file whose growth means the runtime is working.
	RuntimeLog string
}

// Arm installs the watchdog and starts its clock.
//
// The heartbeat file is created here rather than on the first check-in, so the
// deadline runs from the moment the host is armed. A watchdog waiting for a
// first beat would never fire if LARRI died during bring-up — the longest and
// most expensive window there is.
func Arm(ctx context.Context, sess runtime.Session, cfg Config) error {
	if cfg.Deadline < MinDeadline {
		cfg.Deadline = MinDeadline
	}
	// Two commands, and the split is not stylistic.
	//
	// The install command has to name the watchdog — `sh watchdog.sh
	// larri-watchdog` is how the process becomes findable — so its own
	// command line contains that string. A pkill in the *same* command
	// therefore matches the shell issuing it, and the arm kills itself
	// before it has installed anything. It did exactly that on a live host,
	// exiting with status 143.
	//
	// The bracket in the pattern is not enough on its own here: it stops the
	// pattern text from matching, not the plain marker sitting a few lines
	// below it. Two commands, and neither can kill the other's shell.
	_, _ = sess.Run(ctx, killCmd)
	if _, err := sess.Run(ctx, armCmd(cfg)); err != nil {
		return errs.Newf(errs.ClassHostFailure, "deadman.Arm", "install watchdog: %v", err)
	}
	return nil
}

// Beat tells the host that LARRI is still here.
func Beat(ctx context.Context, sess runtime.Session) error {
	if _, err := sess.Run(ctx, "touch "+beatPath+" 2>/dev/null"); err != nil {
		return errs.Newf(errs.ClassHostFailure, "deadman.Beat", "heartbeat: %v", err)
	}
	return nil
}

// Disarm stops the watchdog.
//
// Not needed on the teardown path: destroying an instance takes the watchdog
// with it, and Down has no SSH session anyway — teardown has never depended on
// one (FR-SEC-18). This is for the case where an operator deliberately walks
// away from a rig they intend to keep, and wants the host to stop expecting
// them.
func Disarm(ctx context.Context, sess runtime.Session) error {
	_, _ = sess.Run(ctx, disarmCmd)
	_, _ = sess.Run(ctx, killCmd)
	return nil
}

// Status reports what the host thinks, for surfacing to the operator.
func Status(ctx context.Context, sess runtime.Session) (string, error) {
	out, err := sess.Run(ctx, statusCmd)
	if err != nil {
		return "", errs.Newf(errs.ClassHostFailure, "deadman.Status", "%v", err)
	}
	return string(out), nil
}

// armCmd writes the watchdog and starts it detached.
//
// setsid, so it outlives the SSH session that installed it — which is the
// entire point. Re-arming replaces rather than duplicates: two watchdogs
// racing on one heartbeat would halt a host twice as eagerly for no benefit.
func armCmd(cfg Config) string {
	log := cfg.RuntimeLog
	if log == "" {
		log = "/dev/null"
	}
	return fmt.Sprintf(`set -e
mkdir -p %[1]s
cat > %[2]s <<'LARRI_WATCHDOG_EOF'
%[3]s
LARRI_WATCHDOG_EOF
chmod +x %[2]s
touch %[4]s
LARRI_DEADLINE=%[5]d LARRI_PORT=%[6]d LARRI_RUNTIME_LOG=%[7]s LARRI_MAX_GRACE=%[8]d \
  setsid nohup sh %[2]s larri-watchdog >>%[9]s 2>&1 &
sleep 1
true`, Dir, scriptPath, script, beatPath,
		int(cfg.Deadline.Seconds()), cfg.RuntimePort, log,
		int(MaxGrace.Seconds()), LogPath)
}

// killCmd stops any running watchdog. Its own command, containing the
// bracketed pattern and nothing that the pattern matches.
const killCmd = `pkill -f '[l]arri-watchdog' >/dev/null 2>&1; sleep 1; true`

// disarmCmd removes the heartbeat, which the watchdog reads as "stand down".
// Kept separate from killCmd for the same reason killCmd is separate from the
// install.
const disarmCmd = `rm -f ` + beatPath + ` >/dev/null 2>&1; true`

const statusCmd = `if pgrep -f '[l]arri-watchdog' >/dev/null 2>&1; then ` +
	`echo "armed, last beat $(( $(date +%s) - $(stat -c %Y ` + beatPath + ` 2>/dev/null || date +%s) ))s ago"; ` +
	`else echo "not armed"; fi`
