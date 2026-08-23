// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package deadman

import (
	"context"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type recSession struct {
	mu   sync.Mutex
	cmds []string
	out  string
}

func (s *recSession) Run(_ context.Context, cmd string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cmds = append(s.cmds, cmd)
	return []byte(s.out), nil
}
func (s *recSession) Dial(context.Context, int) (io.ReadWriteCloser, error) { return nil, nil }
func (s *recSession) Close() error                                          { return nil }
func (s *recSession) all() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.cmds, "\n")
}

// The watchdog is a backstop, not a competitor. The local supervisor can tell
// a busy rig from an idle one; the host cannot tell nearly as well. A deadline
// short enough to race it would halt rigs that were merely mid-request.
func TestHostDeadlineAlwaysOutlastsTheLocalTimeout(t *testing.T) {
	for _, idle := range []time.Duration{
		time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour,
	} {
		d := Deadline(idle)
		if d <= idle {
			t.Errorf("idle %s → host deadline %s; the host would act first", idle, d)
		}
		if d < MinDeadline {
			t.Errorf("idle %s → host deadline %s, below the %s floor", idle, d, MinDeadline)
		}
	}
	if Deadline(0) < MinDeadline {
		t.Error("no idle policy should still leave a floor")
	}
}

// Arming must start the clock, not wait for a first heartbeat. A watchdog that
// waited would never fire if LARRI died during bring-up — the longest and most
// expensive window there is.
func TestArmCreatesTheHeartbeatImmediately(t *testing.T) {
	s := &recSession{}
	if err := Arm(context.Background(), s, Config{Deadline: time.Hour, RuntimePort: 8000}); err != nil {
		t.Fatal(err)
	}
	cmd := s.all()
	if !strings.Contains(cmd, "touch "+beatPath) {
		t.Error("the heartbeat file is not created at arm time")
	}
	if !strings.Contains(cmd, "setsid") {
		t.Error("the watchdog would die with the ssh session that installed it")
	}
	if !strings.Contains(cmd, "pkill -f '[l]arri-watchdog'") {
		t.Error("re-arming would leave two watchdogs racing on one heartbeat")
	}
}

// The floor is enforced where it is used, not merely documented.
func TestArmRefusesAnImpatientDeadline(t *testing.T) {
	s := &recSession{}
	if err := Arm(context.Background(), s, Config{Deadline: time.Second}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.all(), "LARRI_DEADLINE=900") {
		t.Errorf("a one-second deadline was not raised to the floor:\n%s", s.all())
	}
}

// No command may carry a process pattern that matches its own command line.
//
// Third outing for this bug, and the first version of this test missed it by
// looking for an unbracketed pattern. The real failure is subtler: the install
// command has to name the watchdog — `sh watchdog.sh larri-watchdog` is what
// makes the process findable — so a pkill in the same command matched the
// shell issuing it, and the arm killed itself before installing anything.
// Status 143, on a live host.
//
// The bracket stops the *pattern* text from matching. It does nothing about a
// plain copy of the target sitting a few lines below. So the invariant is
// stated over the whole command: extract what each pattern would match, and
// check the command does not contain it.
func TestNoCommandKillsItsOwnShell(t *testing.T) {
	cmds := map[string]string{
		"arm":    armCmd(Config{Deadline: time.Hour}),
		"kill":   killCmd,
		"disarm": disarmCmd,
		"status": statusCmd,
		"script": script,
	}
	pat := regexp.MustCompile(`p(?:kill|grep) -f '([^']+)'`)
	for name, c := range cmds {
		for _, m := range pat.FindAllStringSubmatch(c, -1) {
			pattern := m[1]
			// What this pattern actually matches: undo the bracket that
			// makes it self-avoiding, and the regex escaping.
			literal := strings.NewReplacer("[", "", "]", "", `\.`, ".").Replace(pattern)
			// Remove the patterns themselves before looking; a pattern is
			// allowed to appear, that is the whole trick.
			body := c
			for _, m2 := range pat.FindAllString(c, -1) {
				body = strings.ReplaceAll(body, m2, "")
			}
			if strings.Contains(body, literal) {
				t.Errorf("%s greps for %q, which matches %q elsewhere in the same "+
					"command — so it targets the shell that issues it", name, pattern, literal)
			}
		}
	}
	// And the install must still name the watchdog, or nothing can find it
	// later — which is exactly why the kill cannot live in the same command.
	if !strings.Contains(cmds["arm"], "larri-watchdog") {
		t.Error("the installed process is not named, so it could never be found or stopped")
	}
	// Bracketed, which is the whole trick: the pattern matches the watchdog
	// without the killing shell matching itself.
	if !strings.Contains(killCmd, "[l]arri-watchdog") {
		t.Error("the kill does not target the watchdog at all")
	}
}

// The script has to run on whatever minimal image a marketplace host provides.
// A dependency on bash or python is one that will one day not be there, on the
// one code path whose job is to work after everything else has failed.
func TestScriptIsPortableShell(t *testing.T) {
	if !strings.HasPrefix(script, "#!/bin/sh") {
		t.Error("not a POSIX sh script")
	}
	for _, bashism := range []string{"[[ ", "function ", "$'", "declare ", "local "} {
		if strings.Contains(script, bashism) {
			t.Errorf("script uses %q, which dash and busybox ash do not have", bashism)
		}
	}
}

// The claim has to stay inside what was demonstrated. Two live runs watched a
// halted container keep reporting `running`, so a message promising the bill
// had stopped would be telling an operator the opposite of the truth about
// money.
func TestScriptDoesNotClaimToSettleTheBill(t *testing.T) {
	if !strings.Contains(script, "billing may continue") {
		t.Error("the halt message claims more than two live runs could demonstrate")
	}
	for _, overclaim := range []string{"billing ends here", "compute billing ends"} {
		if strings.Contains(script, overclaim) {
			t.Errorf("script claims %q, which is not established on vast", overclaim)
		}
	}
}
