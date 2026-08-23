// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package lint

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const (
	reqsPath  = "../../docs/LARRI_Requirements_Specification.md"
	statePath = "../../docs/PROJECT_STATE.md"
)

var (
	reqRow   = regexp.MustCompile(`(?m)^\| ((?:FR|NFR)-[A-Z]*-?[0-9]+) \| [^|]+ \| ` + "`" + `(\w+)` + "`" + ` \|`)
	anyReqID = regexp.MustCompile(`(?m)^\| ((?:FR|NFR)-[A-Z]*-?[0-9]+) \|`)
	legal    = map[string]bool{"live": true, "done": true, "part": true, "plan": true}
)

func readReqs(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(reqsPath)
	if err != nil {
		t.Skipf("requirements not readable from here: %v", err)
	}
	return string(b)
}

// A status column is only worth having if every requirement has one. A blank
// is indistinguishable from "nobody has looked", which is exactly the state
// the column exists to make visible.
func TestEveryRequirementHasAStatus(t *testing.T) {
	src := readReqs(t)
	all := anyReqID.FindAllStringSubmatch(src, -1)
	withStatus := map[string]string{}
	for _, m := range reqRow.FindAllStringSubmatch(src, -1) {
		withStatus[m[1]] = m[2]
	}
	if len(all) == 0 {
		t.Fatal("no requirement rows found; the guard has stopped guarding")
	}
	for _, m := range all {
		st, ok := withStatus[m[1]]
		if !ok {
			t.Errorf("%s has no status", m[1])
			continue
		}
		if !legal[st] {
			t.Errorf("%s has status %q, which is not one of live/done/part/plan", m[1], st)
		}
	}
}

func TestRequirementIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range anyReqID.FindAllStringSubmatch(readReqs(t), -1) {
		if seen[m[1]] {
			t.Errorf("%s appears twice", m[1])
		}
		seen[m[1]] = true
	}
}

// PROJECT_STATE.md is generated from the status column, so its totals are a
// claim about the other document. A summary that drifts is worse than none:
// it is the page someone reads instead of counting.
func TestProjectStateTotalsMatchTheRequirements(t *testing.T) {
	src := readReqs(t)
	counts := map[string]int{}
	total := 0
	for _, m := range reqRow.FindAllStringSubmatch(src, -1) {
		counts[m[2]]++
		total++
	}
	b, err := os.ReadFile(statePath)
	if err != nil {
		t.Skipf("project state not readable: %v", err)
	}
	state := string(b)

	// The total row: | **Total** | **161** | **57** | **45** | **19** | **40** | ... |
	row := regexp.MustCompile(`\|\s*\*\*Total\*\*\s*\|\s*\*\*(\d+)\*\*\s*\|\s*\*\*(\d+)\*\*\s*\|` +
		`\s*\*\*(\d+)\*\*\s*\|\s*\*\*(\d+)\*\*\s*\|\s*\*\*(\d+)\*\*\s*\|`).FindStringSubmatch(state)
	if row == nil {
		t.Fatal("PROJECT_STATE.md has no total row to check")
	}
	want := []struct {
		name string
		got  int
	}{
		{"total", total}, {"live", counts["live"]}, {"done", counts["done"]},
		{"part", counts["part"]}, {"plan", counts["plan"]},
	}
	for i, w := range want {
		n, _ := strconv.Atoi(row[i+1])
		if n != w.got {
			t.Errorf("PROJECT_STATE.md says %s=%d, the requirements say %d", w.name, n, w.got)
		}
	}
}

// Every `part` must say what is missing, or the status is a shrug. The list in
// PROJECT_STATE.md is where that lives.
func TestEveryPartialNamesItsGap(t *testing.T) {
	src := readReqs(t)
	b, err := os.ReadFile(statePath)
	if err != nil {
		t.Skipf("project state not readable: %v", err)
	}
	state := string(b)
	for _, m := range reqRow.FindAllStringSubmatch(src, -1) {
		if m[2] != "part" {
			continue
		}
		if !strings.Contains(state, "| "+m[1]+" |") {
			t.Errorf("%s is `part` but PROJECT_STATE.md does not say what is missing", m[1])
		}
	}
}
