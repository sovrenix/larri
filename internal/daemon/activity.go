// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.sovrenix.com/larri/internal/runtime"
)

// hostCounters is a point-in-time reading of a host's activity counters.
//
// Three independent signals, and the independence is the point. A single one
// answers the wrong question:
//
//   - **Network alone is not liveness.** An image that has finished
//     downloading spends minutes extracting layers with the network idle, and
//     a model loading weights into VRAM moves disk and CPU while the wire is
//     silent. A network-only probe would call all of that dead.
//   - **CPU alone is not liveness either.** A host waiting on a slow registry
//     is nearly idle while making real progress.
//   - **Disk alone misses** a download still buffering.
//
// So the probe asks whether *anything* is moving, and reports which, because
// which one is moving tells the operator what phase the host is actually in.
type hostCounters struct {
	At       time.Time
	CPUBusy  uint64 // jiffies spent not idle
	CPUTotal uint64
	DiskRead uint64 // sectors
	DiskWrit uint64
	NetRx    uint64 // bytes
	NetTx    uint64
	ok       bool
}

// countersCmd reads all three counters in one round trip.
//
// One command rather than three, because each SSH exec is a round trip and the
// operator is paying for the seconds. Failures are swallowed with `|| true` so
// a host missing one of these files still yields the other two.
const countersCmd = `
{ awk '/^cpu /{t=0; for(i=2;i<=NF;i++) t+=$i; print "cpu", t, $5}' /proc/stat 2>/dev/null || true
  awk '{r+=$6; w+=$10} END{print "disk", r+0, w+0}' /proc/diskstats 2>/dev/null || true
  awk 'NR>2{rx+=$2; tx+=$10} END{print "net", rx+0, tx+0}' /proc/net/dev 2>/dev/null || true
} 2>/dev/null`

func readCounters(ctx context.Context, sess runtime.Session) hostCounters {
	c := hostCounters{At: time.Now()}
	out, err := sess.Run(ctx, countersCmd)
	if err != nil && len(out) == 0 {
		return c
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		a, _ := strconv.ParseUint(f[1], 10, 64)
		b, _ := strconv.ParseUint(f[2], 10, 64)
		switch f[0] {
		case "cpu":
			c.CPUTotal, c.CPUBusy = a, a-b // total, total-idle
			c.ok = true
		case "disk":
			c.DiskRead, c.DiskWrit = a, b
			c.ok = true
		case "net":
			c.NetRx, c.NetTx = a, b
			c.ok = true
		}
	}
	return c
}

// activity summarises what changed between two readings.
type activity struct {
	CPUPercent float64
	DiskMBps   float64
	NetMBps    float64
	Moving     bool
	Valid      bool
}

// since compares two readings.
//
// "Moving" is a disjunction on purpose: any one signal above its floor means
// the host is working. Requiring agreement between them would reproduce the
// single-signal blindness this exists to avoid.
func (c hostCounters) since(prev hostCounters) activity {
	var a activity
	if !c.ok || !prev.ok {
		return a
	}
	secs := c.At.Sub(prev.At).Seconds()
	if secs <= 0 {
		return a
	}
	a.Valid = true
	if dt := c.CPUTotal - prev.CPUTotal; dt > 0 && c.CPUTotal >= prev.CPUTotal {
		a.CPUPercent = float64(c.CPUBusy-prev.CPUBusy) / float64(dt) * 100
	}
	const sectorBytes = 512
	if c.DiskRead+c.DiskWrit >= prev.DiskRead+prev.DiskWrit {
		d := (c.DiskRead + c.DiskWrit) - (prev.DiskRead + prev.DiskWrit)
		a.DiskMBps = float64(d) * sectorBytes / secs / (1 << 20)
	}
	if c.NetRx+c.NetTx >= prev.NetRx+prev.NetTx {
		n := (c.NetRx + c.NetTx) - (prev.NetRx + prev.NetTx)
		a.NetMBps = float64(n) / secs / (1 << 20)
	}
	// Floors sit above idle chatter: a booted host ticks over at a few percent
	// CPU and a trickle of packets without doing anything for the operator.
	a.Moving = a.CPUPercent > 5 || a.DiskMBps > 0.5 || a.NetMBps > 0.2
	return a
}

func (a activity) String() string {
	if !a.Valid {
		return "host counters unavailable"
	}
	return fmt.Sprintf("cpu %.0f%% · disk %.1f MB/s · net %.1f MB/s",
		a.CPUPercent, a.DiskMBps, a.NetMBps)
}

// describes what the mix of signals suggests, so a long wait reads as a phase
// rather than as a number.
func (a activity) phase() string {
	switch {
	case !a.Valid || !a.Moving:
		return ""
	case a.NetMBps > 1 && a.DiskMBps > 0.5:
		return "downloading"
	case a.NetMBps > 1:
		return "transferring"
	case a.DiskMBps > 1:
		return "unpacking or loading from disk"
	case a.CPUPercent > 20:
		return "working"
	default:
		return "active"
	}
}

// watchHostActivity reports whether a host is doing anything, on an interval,
// until the context ends.
//
// This is a **supervision probe, not a metrics collector** (§12.3). The
// distinction matters because they have opposite failure semantics: the
// collector may fail and nothing may depend on it, while this is allowed to
// inform a decision. Sharing the SSH connection is fine; sharing ownership is
// not.
func (o *Orchestrator) watchHostActivity(ctx context.Context, sess runtime.Session,
	every time.Duration, onIdle func(idleFor time.Duration)) {

	if every <= 0 {
		every = 20 * time.Second
	}
	prev := readCounters(ctx, sess)
	lastMoved := time.Now()
	t := time.NewTicker(every)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cur := readCounters(ctx, sess)
		a := cur.since(prev)
		prev = cur

		switch {
		case !a.Valid:
			// Counters unreadable is not evidence of a stuck host, so it is
			// reported and otherwise ignored.
			o.emit("host", "%s", a)
		case a.Moving:
			lastMoved = time.Now()
			if p := a.phase(); p != "" {
				o.emit("host", "%s (%s)", a, p)
			} else {
				o.emit("host", "%s", a)
			}
		default:
			idle := time.Since(lastMoved)
			o.emit("host", "%s — idle for %s", a, idle.Round(time.Second))
			if onIdle != nil {
				onIdle(idle)
			}
		}
	}
}
