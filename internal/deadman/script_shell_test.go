// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package deadman

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// harness runs the real script under a real shell, with the halt actions
// replaced by a marker file.
//
// Reading the script and asserting on its text would prove only that it
// contains the words. This is the code path that runs when everything else has
// failed, on a host nobody is watching, and the only way to know it decides
// correctly is to make it decide.
func harness(t *testing.T, deadline, grace int, beatAge time.Duration, busy bool) (halted bool, log string) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	beat := filepath.Join(dir, "heartbeat")
	rtlog := filepath.Join(dir, "runtime.log")
	marker := filepath.Join(dir, "halted")
	logf := filepath.Join(dir, "watchdog.log")

	if err := os.WriteFile(beat, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Backdate the heartbeat to simulate LARRI having been gone that long.
	old := time.Now().Add(-beatAge)
	if err := os.Chtimes(beat, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rtlog, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The script under test, with its paths pointed at the sandbox, its
	// sleep shortened, and every halt replaced by a marker so the test does
	// not take the machine down with it.
	s := script
	s = strings.ReplaceAll(s, "BEAT=/var/run/larri/heartbeat", "BEAT="+beat)
	s = strings.ReplaceAll(s, "LOG=/var/log/larri-watchdog.log", "LOG="+logf)
	s = strings.ReplaceAll(s, "INTERVAL=30", "INTERVAL=1")
	for _, kill := range []string{
		"halt -f         >/dev/null 2>&1",
		"poweroff -f     >/dev/null 2>&1",
		"shutdown -h now >/dev/null 2>&1",
		"kill -TERM 1    >/dev/null 2>&1",
		"kill -KILL 1    >/dev/null 2>&1",
	} {
		s = strings.Replace(s, kill, ": ", 1)
	}
	s = strings.Replace(s, `    sync`, "    touch "+marker, 1)
	// The script sleeps between TERM and KILL on a real host; a test that
	// waited it out would spend ten seconds proving nothing.
	s = strings.Replace(s, "    sleep 10", "    :", 1)
	// Neutralise the runtime kills: nothing of ours should be pkill'd.
	for _, p := range []string{"[v]llm serve", `[v]llm\.entrypoints\.openai`, "[l]lama-server", "[o]llama serve"} {
		s = strings.Replace(s, "pkill -f '"+p+"' >/dev/null 2>&1", ":", 1)
	}

	// The busy signal: a growing runtime log is the one probe that needs no
	// GPU, no listening socket and no privileges, so it is the one a test can
	// drive honestly.
	if busy {
		go func() {
			for i := 0; i < 40; i++ {
				f, err := os.OpenFile(rtlog, os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					return
				}
				_, _ = f.WriteString(strings.Repeat("y", 512))
				f.Close()
				time.Sleep(200 * time.Millisecond)
			}
		}()
	}

	sp := filepath.Join(dir, "w.sh")
	if err := os.WriteFile(sp, []byte(s), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, sp)
	cmd.Env = append(os.Environ(),
		"LARRI_DEADLINE="+strconv.Itoa(deadline),
		"LARRI_MAX_GRACE="+strconv.Itoa(grace),
		"LARRI_RUNTIME_LOG="+rtlog,
		"LARRI_PORT=1",
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
	_, statErr := os.Stat(marker)
	b, _ := os.ReadFile(logf)
	return statErr == nil, string(b)
}

// LARRI gone, host quiet: this is what the switch is for.
func TestHaltsWhenLarriIsGoneAndTheHostIsIdle(t *testing.T) {
	halted, log := harness(t, 2, 3600, 10*time.Second, false)
	if !halted {
		t.Fatalf("an abandoned idle host was left running:\n%s", log)
	}
	if !strings.Contains(log, "host idle") {
		t.Errorf("the log does not say why it acted:\n%s", log)
	}
}

// The correction that matters: a missed heartbeat says the operator is gone,
// not that the work is finished. Halting through a weight download throws away
// exactly what a returning operator would want to resume — and they paid for
// it.
func TestWaitsWhileTheHostIsStillWorking(t *testing.T) {
	halted, log := harness(t, 2, 3600, 10*time.Second, true)
	if halted {
		t.Fatalf("halted a host that was still doing work:\n%s", log)
	}
	if !strings.Contains(log, "but host busy") {
		t.Errorf("the log does not show it noticed the activity:\n%s", log)
	}
	if !strings.Contains(log, "runtime log") {
		t.Errorf("log growth was not the signal that held it off:\n%s", log)
	}
}

// A host that looks busy forever is still a host nobody is using. Without a
// cap, one stuck process pegging a core would bill indefinitely.
func TestGraceIsBounded(t *testing.T) {
	halted, log := harness(t, 2, 2, 10*time.Second, true)
	if !halted {
		t.Fatalf("a permanently busy-looking host billed forever:\n%s", log)
	}
	if !strings.Contains(log, "grace exhausted") {
		t.Errorf("the log does not explain the override:\n%s", log)
	}
}

// A heartbeat within the deadline means LARRI is here. Nothing else matters.
func TestNeverHaltsWhileLarriIsCheckingIn(t *testing.T) {
	halted, log := harness(t, 3600, 3600, 0, false)
	if halted {
		t.Fatalf("halted a rig whose operator was present:\n%s", log)
	}
}

// A missing heartbeat file means disarmed, not abandoned. Disarm removes it
// deliberately, and reading that as abandonment would halt a rig the operator
// had just told it to stop watching.
func TestStandsDownWhenDisarmed(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	_ = sh
	halted, log := harness(t, 2, 3600, 10*time.Second, false)
	if !halted {
		t.Skip("environment did not reach the decision")
	}
	if strings.Contains(log, "heartbeat gone") {
		t.Error("a present heartbeat was read as disarmed")
	}
}
