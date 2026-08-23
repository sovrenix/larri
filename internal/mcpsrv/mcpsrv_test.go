// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package mcpsrv

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/tools"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	r := tools.NewRegistry()
	r.MustAdd(tools.Tool{
		Name: "larri_status", Description: "read-only", Schema: tools.Object(nil),
		Handler: func(context.Context, json.RawMessage) (any, error) {
			return map[string]any{"rigs": []any{}}, nil
		},
	})
	r.MustAdd(tools.Tool{
		Name: "larri_up", Description: "SPENDS MONEY", Schema: tools.Object(nil),
		Consequential: true, Exposure: tools.ExposeMCPOnly,
		Handler: func(context.Context, json.RawMessage) (any, error) {
			return map[string]any{"price_hr": 0.44}, nil
		},
	})
	r.MustAdd(tools.Tool{
		Name: "larri_boom", Description: "fails", Schema: tools.Object(nil),
		Handler: func(context.Context, json.RawMessage) (any, error) {
			return nil, context.DeadlineExceeded
		},
	})
	return &Server{Registry: r, Name: "larri", Version: "test"}
}

func roundtrip(t *testing.T, s *Server, lines ...string) []map[string]any {
	t.Helper()
	var out strings.Builder
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("undecodable reply %q: %v", l, err)
		}
		got = append(got, m)
	}
	return got
}

func TestInitializeAnnouncesTools(t *testing.T) {
	got := roundtrip(t, testServer(t),
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if len(got) != 1 {
		t.Fatalf("want 1 reply, got %d", len(got))
	}
	res := got[0]["result"].(map[string]any)
	if res["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion = %v", res["protocolVersion"])
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Error("tools capability not announced")
	}
}

// A notification carries no id and takes no reply. Answering one desyncs some
// hosts fatally, so this is not a stylistic point.
func TestNotificationsAreNotAnswered(t *testing.T) {
	got := roundtrip(t, testServer(t),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	if len(got) != 1 {
		t.Fatalf("want exactly 1 reply (the ping), got %d", len(got))
	}
	if got[0]["id"].(float64) != 7 {
		t.Errorf("replied to the wrong message: %v", got[0]["id"])
	}
}

// The agent must be able to tell a rental from a read before it calls one.
func TestConsequentialToolsAreAnnotated(t *testing.T) {
	got := roundtrip(t, testServer(t), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	list := got[0]["result"].(map[string]any)["tools"].([]any)

	byName := map[string]map[string]any{}
	for _, raw := range list {
		d := raw.(map[string]any)
		byName[d["name"].(string)] = d
	}
	up, ok := byName["larri_up"]
	if !ok {
		t.Fatal("larri_up missing from the tool list")
	}
	ann := up["annotations"].(map[string]any)
	if ann["destructiveHint"] != true {
		t.Error("a tool that rents hardware is not marked destructive")
	}
	if !strings.Contains(up["description"].(string), "SPENDS MONEY") {
		t.Error("the cost implication is not in the description")
	}
	status := byName["larri_status"]
	if status["annotations"].(map[string]any)["readOnlyHint"] != true {
		t.Error("a read-only tool is not marked read-only")
	}
}

func TestCallReturnsToolOutput(t *testing.T) {
	got := roundtrip(t, testServer(t),
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"larri_status","arguments":{}}}`)
	res := got[0]["result"].(map[string]any)
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "rigs") {
		t.Errorf("tool output not returned: %s", text)
	}
}

// A failed rental is something the agent should read and reason about; a
// protocol error is something it should give up on. Conflating them makes a
// recoverable failure look fatal.
func TestToolFailureIsAResultNotAProtocolError(t *testing.T) {
	got := roundtrip(t, testServer(t),
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"larri_boom","arguments":{}}}`)
	if _, isRPCErr := got[0]["error"]; isRPCErr {
		t.Fatal("a tool failure was reported as a protocol error")
	}
	res := got[0]["result"].(map[string]any)
	if res["isError"] != true {
		t.Error("the failure was not flagged to the agent")
	}
}

func TestUnknownToolIsRefused(t *testing.T) {
	got := roundtrip(t, testServer(t),
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"larri_nope","arguments":{}}}`)
	if _, ok := got[0]["error"]; !ok {
		t.Error("an unknown tool was not refused")
	}
}

func TestBadJSONDoesNotKillTheSession(t *testing.T) {
	got := roundtrip(t, testServer(t),
		`{not json`,
		`{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	if len(got) != 2 {
		t.Fatalf("want a parse error and then a working ping, got %d replies", len(got))
	}
	if got[1]["id"].(float64) != 9 {
		t.Error("the session did not recover from malformed input")
	}
}
