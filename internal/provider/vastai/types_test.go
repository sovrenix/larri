// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package vastai

import (
	"encoding/json"
	"strings"
	"testing"
)

// The body of a real instance Vast abandoned mid-boot, field-for-field as the
// API returned it. actual_status reads "created" — indistinguishable from a
// boot in progress — while the contract has already given up, which is the
// pair LARRI needs in order to tell the two apart.
func TestAbandonedInstanceCarriesTheProvidersIntent(t *testing.T) {
	const body = `{
	  "id": 48738927,
	  "actual_status": "created",
	  "cur_state": "stopped",
	  "next_state": "stopped",
	  "intended_status": "stopped",
	  "status_msg": "Error response from daemon: failed to create task for container: failed to create shim task: OCI runtime create failed: could not apply required modification to OCI specification: error modifying OCI spec: failed to inject CDI devices: unresolvable CDI devices",
	  "ssh_host": "ssh4.vast.ai",
	  "ssh_port": 18926,
	  "machine_id": 145651,
	  "dph_total": 0.017,
	  "start_date": 1787722703.0
	}`
	var raw wireInstance
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	got, err := raw.normalise()
	if err != nil {
		t.Fatal(err)
	}
	if got.Intent != "stopped" {
		t.Fatalf("Intent = %q, want stopped — without it the boot wait never learns the host was abandoned", got.Intent)
	}
	if got.Status != "created" {
		t.Errorf("Status = %q, want the container's own account", got.Status)
	}
	if got.Running {
		t.Error("an abandoned instance is not running")
	}
	if !strings.Contains(got.StatusMsg, "unresolvable CDI devices") {
		t.Errorf("the message naming the cause must survive: %q", got.StatusMsg)
	}
}

// intended_status is preferred, but next_state carries the same news when it
// is absent.
func TestIntentFallsBackToNextState(t *testing.T) {
	var raw wireInstance
	if err := json.Unmarshal([]byte(`{"id":1,"actual_status":"loading","next_state":"stopped"}`), &raw); err != nil {
		t.Fatal(err)
	}
	got, err := raw.normalise()
	if err != nil {
		t.Fatal(err)
	}
	if got.Intent != "stopped" {
		t.Errorf("Intent = %q, want stopped from next_state", got.Intent)
	}
}
