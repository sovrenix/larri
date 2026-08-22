// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"testing"
	"time"
)

func at(t time.Time, cpuTotal, cpuBusy, dRead, dWrit, rx, tx uint64) hostCounters {
	return hostCounters{
		At: t, CPUTotal: cpuTotal, CPUBusy: cpuBusy,
		DiskRead: dRead, DiskWrit: dWrit, NetRx: rx, NetTx: tx, ok: true,
	}
}

// The constraint that shapes this: network alone answers the wrong question.
// Each phase of a bring-up moves a different counter, and a probe watching one
// of them calls the others dead.
func TestAnySignalCountsAsAlive(t *testing.T) {
	t0 := time.Now()
	t1 := t0.Add(10 * time.Second)
	base := at(t0, 1000, 100, 0, 0, 0, 0)

	cases := map[string]hostCounters{
		// Pulling an image: the wire is busy.
		"downloading": at(t1, 2000, 200, 0, 0, 200<<20, 0),
		// Extracting layers: network idle, disk hammering. A network-only
		// probe would call this stuck.
		"unpacking": at(t1, 2000, 900, 400_000, 400_000, 0, 0),
		// Loading weights into VRAM: CPU and disk, no network at all.
		"loading": at(t1, 2000, 1000, 200_000, 0, 0, 0),
	}
	for name, cur := range cases {
		a := cur.since(base)
		if !a.Valid {
			t.Fatalf("%s: reading should be valid", name)
		}
		if !a.Moving {
			t.Errorf("%s: host is working but was judged idle (%s)", name, a)
		}
	}
}

// A booted host ticks over without doing anything for the operator, so the
// floors have to sit above idle chatter.
func TestIdleHostIsJudgedIdle(t *testing.T) {
	t0 := time.Now()
	// 1% cpu, a few KB of disk, a trickle of packets.
	prev := at(t0, 100_000, 10_000, 1_000, 1_000, 1_000_000, 500_000)
	cur := at(t0.Add(20*time.Second), 102_000, 10_020, 1_020, 1_020, 1_001_000, 500_500)
	a := cur.since(prev)
	if !a.Valid {
		t.Fatal("should be valid")
	}
	if a.Moving {
		t.Errorf("idle chatter judged as work: %s", a)
	}
}

// Counters that cannot be read are not evidence of a stuck host.
func TestUnreadableCountersAreNotIdle(t *testing.T) {
	a := hostCounters{}.since(hostCounters{})
	if a.Valid {
		t.Error("a failed reading must not produce a verdict")
	}
	if a.Moving {
		t.Error("nor should it claim movement")
	}
	if got := a.String(); got == "" {
		t.Error("it should still describe itself")
	}
}

// A counter that goes backwards — a reboot, a namespace change — must not
// produce a nonsense rate.
func TestCountersGoingBackwardsDoNotPanicOrLie(t *testing.T) {
	t0 := time.Now()
	prev := at(t0, 100_000, 50_000, 900_000, 900_000, 9<<30, 9<<30)
	cur := at(t0.Add(10*time.Second), 1_000, 500, 10, 10, 100, 100) // reset
	a := cur.since(prev)
	if a.DiskMBps < 0 || a.NetMBps < 0 || a.CPUPercent < 0 {
		t.Errorf("negative rates from a counter reset: %s", a)
	}
}

// Which signal is moving tells the operator what the host is actually doing,
// which is more useful than a number.
func TestPhaseNamesWhatIsHappening(t *testing.T) {
	t0 := time.Now()
	t1 := t0.Add(10 * time.Second)
	base := at(t0, 1000, 100, 0, 0, 0, 0)

	if p := at(t1, 2000, 300, 100_000, 0, 500<<20, 0).since(base).phase(); p != "downloading" {
		t.Errorf("network + disk should read as downloading, got %q", p)
	}
	if p := at(t1, 2000, 1500, 500_000, 500_000, 0, 0).since(base).phase(); p != "unpacking or loading from disk" {
		t.Errorf("disk-only should read as unpacking, got %q", p)
	}
	if p := base.since(base).phase(); p != "" {
		t.Errorf("no movement has no phase, got %q", p)
	}
}
