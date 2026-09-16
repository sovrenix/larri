// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package wire

import (
	"errors"
	"testing"

	"go.sovrenix.com/larri/internal/core"
)

// fakeClient records what the protocol did to it.
type fakeClient struct {
	name      string
	tier      Tier
	present   bool
	detectErr error
	applyErr  error
	revertErr error

	applied  int
	reverted int
}

func (f *fakeClient) Name() string { return f.name }
func (f *fakeClient) Tier() Tier   { return f.tier }
func (f *fakeClient) Detect() (bool, error) {
	return f.present, f.detectErr
}

func (f *fakeClient) Apply(Endpoint) (core.WiringRecord, error) {
	f.applied++
	if f.applyErr != nil {
		return core.WiringRecord{}, f.applyErr
	}
	return core.WiringRecord{Path: "/cfg/" + f.name, BackupPath: "/cfg/" + f.name + ".bak"}, nil
}

func (f *fakeClient) Revert(core.WiringRecord) error {
	f.reverted++
	return f.revertErr
}

var _ ClientWriter = (*fakeClient)(nil)

func ep() Endpoint { return Endpoint{URL: "http://127.0.0.1:8000/v1", Model: "m"} }

// Never write config for a client that is not present (§10.2 step 1). The
// failure this prevents is creating a config file for an application the
// operator does not have, which they then find and cannot explain.
func TestAnAbsentClientIsNotWritten(t *testing.T) {
	absent := &fakeClient{name: "absent", tier: TierFile, present: false}
	here := &fakeClient{name: "here", tier: TierFile, present: true}

	recs, errs := Apply([]ClientWriter{absent, here}, ep(), nil)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if absent.applied != 0 {
		t.Error("wrote config for a client that is not installed")
	}
	if here.applied != 1 {
		t.Errorf("present client applied %d times, want 1", here.applied)
	}
	if len(recs) != 1 || recs[0].Client != "here" {
		t.Errorf("records = %+v", recs)
	}
}

// Tier B and C are never written directly. Editing a datastore belonging to a
// running application is how chat history gets corrupted to save four clicks.
func TestOnlyTierAIsWritten(t *testing.T) {
	for _, tier := range []Tier{TierAppStore, TierGuided} {
		c := &fakeClient{name: "c", tier: tier, present: true}
		recs, errs := Apply([]ClientWriter{c}, ep(), nil)
		if len(errs) != 0 {
			t.Fatalf("tier %s: errors: %v", tier, errs)
		}
		if c.applied != 0 {
			t.Errorf("tier %s was written to directly", tier)
		}
		// A record is still produced: the operator was told to do something,
		// and the tier belongs in the rig's history either way.
		if len(recs) != 1 || recs[0].Tier != string(tier) {
			t.Errorf("tier %s: records = %+v", tier, recs)
		}
	}
}

// A failed write must not fail the rig. A rig that serves but could not edit an
// IDE config is still a rig (§16, ClassWiring).
func TestAFailedApplyIsReportedAndSurvived(t *testing.T) {
	bad := &fakeClient{name: "bad", tier: TierFile, present: true, applyErr: errors.New("permission denied")}
	good := &fakeClient{name: "good", tier: TierFile, present: true}

	recs, errs := Apply([]ClientWriter{bad, good}, ep(), nil)
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly the failed write", errs)
	}
	if good.applied != 1 {
		t.Error("one client failing stopped the others being wired")
	}
	if len(recs) != 1 || recs[0].Client != "good" {
		t.Errorf("a failed write left a record to revert: %+v", recs)
	}
}

// Verification applies to every tier: a file written to an application that
// never re-read it is indistinguishable from one never written.
func TestTheProbeDecidesVerified(t *testing.T) {
	c := &fakeClient{name: "c", tier: TierFile, present: true}

	recs, _ := Apply([]ClientWriter{c}, ep(), func(string) (bool, error) { return true, nil })
	if len(recs) != 1 || !recs[0].Verified {
		t.Errorf("a successful probe did not mark the record verified: %+v", recs)
	}

	recs, _ = Apply([]ClientWriter{c}, ep(), func(string) (bool, error) { return false, nil })
	if len(recs) != 1 || recs[0].Verified {
		t.Error("a failed probe still reported the client verified")
	}

	// A probe that errors is reported, and the record still stands: something
	// was written and it still has to be reverted.
	recs, errs := Apply([]ClientWriter{c}, ep(), func(string) (bool, error) {
		return false, errors.New("unreachable")
	})
	if len(errs) != 1 {
		t.Errorf("a probe error was not reported: %v", errs)
	}
	if len(recs) != 1 {
		t.Error("a probe error discarded the record that has to be reverted")
	}
}

// A torn-down rig never leaves an IDE pointing at a dead endpoint
// (FR-WIRE-05), so every record is attempted even when an earlier one fails.
func TestRevertAttemptsEveryRecord(t *testing.T) {
	bad := &fakeClient{name: "bad", tier: TierFile, present: true, revertErr: errors.New("gone")}
	good := &fakeClient{name: "good", tier: TierFile, present: true}
	ws := []ClientWriter{bad, good}

	recs, _ := Apply(ws, ep(), nil)
	errs := Revert(recs, Index(ws))

	if len(errs) != 1 {
		t.Errorf("errors = %v, want exactly the failed revert", errs)
	}
	if bad.reverted != 1 || good.reverted != 1 {
		t.Errorf("reverted bad=%d good=%d; one failure stopped the rest",
			bad.reverted, good.reverted)
	}
}

// Applied in order, undone in reverse: undoing forwards can leave an
// intermediate state neither half expected.
func TestRevertRunsInReverseOrder(t *testing.T) {
	var order []string
	mk := func(name string) *recordingClient {
		return &recordingClient{name: name, order: &order}
	}
	a, b, c := mk("a"), mk("b"), mk("c")
	ws := []ClientWriter{a, b, c}

	recs, _ := Apply(ws, ep(), nil)
	Revert(recs, Index(ws))

	want := []string{"a", "b", "c", "c", "b", "a"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

type recordingClient struct {
	name  string
	order *[]string
}

func (r *recordingClient) Name() string          { return r.name }
func (r *recordingClient) Tier() Tier            { return TierFile }
func (r *recordingClient) Detect() (bool, error) { return true, nil }
func (r *recordingClient) Apply(Endpoint) (core.WiringRecord, error) {
	*r.order = append(*r.order, r.name)
	return core.WiringRecord{}, nil
}
func (r *recordingClient) Revert(core.WiringRecord) error {
	*r.order = append(*r.order, r.name)
	return nil
}

// A record with no writer to undo it must say so and name the backup, rather
// than disappearing. That is the case where the operator has to act.
func TestARecordWithNoWriterNamesTheBackup(t *testing.T) {
	recs := []core.WiringRecord{{
		Client: "vanished", Tier: string(TierFile), BackupPath: "/cfg/vanished.bak",
	}}
	errs := Revert(recs, map[string]ClientWriter{})
	if len(errs) != 1 {
		t.Fatalf("errors = %v", errs)
	}
	if got := errs[0].Error(); !contains(got, "/cfg/vanished.bak") {
		t.Errorf("error does not name the backup: %s", got)
	}
}

// Only the stable loopback URL and served-model name are ever written
// (FR-WIRE-06). The ephemeral provider host must not be expressible here.
func TestTheEndpointCarriesNoProviderHost(t *testing.T) {
	e := ep()
	if e.URL == "" || e.Model == "" {
		t.Fatal("the endpoint carries neither of the two things it may write")
	}
	// The type has no field for a remote host, which is the enforcement:
	// a writer cannot record one because it is never given one.
	if contains(e.URL, "vast") || contains(e.URL, "runpod") {
		t.Error("the endpoint names a provider host")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
