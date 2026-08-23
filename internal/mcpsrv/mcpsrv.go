// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package mcpsrv serves LARRI's tool registry over the Model Context Protocol.
//
// It is an adapter, not a second definition: every tool it exposes comes from
// internal/tools, so a `larri_down` invoked by Claude Code does exactly what a
// `larri_down` invoked anywhere else does (§14.2).
//
// Transport is stdio with newline-delimited JSON-RPC 2.0, which is what agent
// hosts launch a local MCP server as. Nothing listens on a socket: an MCP
// server that accepted network connections would be a second unauthenticated
// path to the operations that spend money.
package mcpsrv

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"go.sovrenix.com/larri/internal/tools"
)

// protocolVersion is the MCP revision this server implements.
const protocolVersion = "2024-11-05"

// Server adapts a tool registry to MCP over a stdio transport.
type Server struct {
	Registry *tools.Registry
	Name     string
	Version  string

	mu  sync.Mutex // serialises writes; handlers may run concurrently
	out *bufio.Writer
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve reads requests until the input closes or the context ends.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = bufio.NewWriter(out)
	sc := bufio.NewScanner(in)
	// Agent hosts send whole messages on one line, and a tool result carrying
	// a runtime log is easily past the default 64 KB.
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)

	for sc.Scan() {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			s.send(response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		// A notification has no id and takes no reply. Answering one is a
		// protocol violation that some hosts treat as a fatal desync.
		if len(req.ID) == 0 {
			continue
		}
		s.send(s.handle(ctx, req))
	}
	return sc.Err()
}

func (s *Server) send(r response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	s.out.Write(b)
	s.out.WriteByte('\n')
	s.out.Flush()
}

func (s *Server) handle(ctx context.Context, req request) response {
	reply := func(result any) response {
		return response{JSONRPC: "2.0", ID: req.ID, Result: result}
	}
	fail := func(code int, msg string) response {
		return response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: msg}}
	}

	switch req.Method {
	case "initialize":
		return reply(map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		})

	case "tools/list":
		return reply(map[string]any{"tools": s.definitions()})

	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fail(-32602, "invalid params")
		}
		if _, ok := s.Registry.Lookup(p.Name); !ok {
			return fail(-32602, "no tool "+p.Name)
		}
		result, err := s.Registry.Call(ctx, p.Name, p.Arguments)
		if err != nil {
			// Reported as a tool result with isError, not as a JSON-RPC
			// error: a failed rental is something the agent should read and
			// reason about, where a protocol error is something it should
			// give up on.
			return reply(errorContent(err))
		}
		return reply(okContent(result))

	case "ping":
		return reply(map[string]any{})

	default:
		return fail(-32601, "method not found: "+req.Method)
	}
}

// definitions renders the registry as MCP tool definitions.
//
// Only the MCP driver's exposure set, and consequential tools are annotated so
// a host can prompt before spending rather than after.
func (s *Server) definitions() []map[string]any {
	list := s.Registry.For(tools.ExposeMCPOnly)
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		def := map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.Schema,
		}
		if t.Consequential {
			def["annotations"] = map[string]any{
				"title":           t.Name,
				"readOnlyHint":    false,
				"destructiveHint": true,
				"openWorldHint":   true,
			}
		} else {
			def["annotations"] = map[string]any{
				"title":        t.Name,
				"readOnlyHint": true,
			}
		}
		out = append(out, def)
	}
	return out
}

func okContent(v any) map[string]any {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errorContent(err)
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(b)}},
	}
}

func errorContent(err error) map[string]any {
	return map[string]any{
		"isError": true,
		"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("larri: %v", err)}},
	}
}
