// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package comfyui is the ComfyUI claw: an image-generation graph engine run on
// a rented GPU.
//
// It is a *remote* claw (§6.8.1) — the application runs on the rented box, so
// whatever it renders exists only there and is collected before the destroy.
// What it stands up is a runtime.Workload and deliberately not a
// runtime.Runtime: there is no /v1 behind this endpoint and nothing may issue a
// completion against it. What it serves instead is ComfyUI's own HTTP API —
// POST /prompt to enqueue a graph, /history to collect what it produced, /view
// to fetch an image — together with the bundled web frontend the operator
// actually looks at.
//
// Everything ComfyUI-specific lives here and in ./workflow. The daemon imports
// neither, and a lint guard says so.
package comfyui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
)

// Client speaks ComfyUI's HTTP API at the local end of the tunnel.
//
// The local end, deliberately, and for the reason readiness is checked there
// for every other workload: a call made on the host would prove only that
// ComfyUI answers itself, while a call through the tunnel proves the forward
// carries traffic, the proxy is substituting credentials, and the graph
// renders. What the operator is about to do is exactly what gets tested.
type Client struct {
	// Addr is host:port of the local listener.
	Addr string

	// Token authenticates to LARRI's proxy. It is a *client* token, never the
	// rig's: the proxy strips it and substitutes its own (FR-SEC-22).
	Token string

	// Probe marks traffic as LARRI's own so it does not reset the idle clock
	// (FR-SUP-08). A supervisor polling /history every few seconds would
	// otherwise hold a rig open forever.
	Probe bool

	HTTP *http.Client
}

// PromptResult is what ComfyUI says about an accepted graph.
type PromptResult struct {
	PromptID   string                     `json:"prompt_id"`
	Number     int                        `json:"number"`
	NodeErrors map[string]json.RawMessage `json:"node_errors"`
}

// OutputFile is one artefact a graph produced.
type OutputFile struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"` // output | temp | input
	NodeID    string `json:"-"`
}

// HistoryEntry is ComfyUI's record of one executed graph.
type HistoryEntry struct {
	Outputs map[string]struct {
		Images []OutputFile `json:"images"`
		GIFs   []OutputFile `json:"gifs"`
	} `json:"outputs"`
	Status struct {
		StatusStr string          `json:"status_str"`
		Completed bool            `json:"completed"`
		Messages  json.RawMessage `json:"messages"`
	} `json:"status"`
}

// Files flattens every artefact in an entry, tagged with the node that made
// it, in a stable order.
func (h HistoryEntry) Files() []OutputFile {
	var out []OutputFile
	for _, node := range sortedKeys(h.Outputs) {
		o := h.Outputs[node]
		for _, f := range o.Images {
			f.NodeID = node
			out = append(out, f)
		}
		for _, f := range o.GIFs {
			f.NodeID = node
			out = append(out, f)
		}
	}
	return out
}

// Failed reports whether ComfyUI finished this graph unsuccessfully.
//
// Completion and success are separate fields and conflating them is how a rig
// reports a clean render for a graph that raised on its first node.
func (h HistoryEntry) Failed() bool {
	return h.Status.Completed && h.Status.StatusStr != "" && h.Status.StatusStr != "success"
}

// SystemStats is ComfyUI's account of the hardware it found.
type SystemStats struct {
	Devices []struct {
		Name      string `json:"name"`
		Type      string `json:"type"`
		VRAMTotal int64  `json:"vram_total"`
		VRAMFree  int64  `json:"vram_free"`
	} `json:"devices"`
}

// HasCUDA reports whether ComfyUI is running on a GPU.
//
// Worth asking separately from "is the server up", because ComfyUI starts
// perfectly well with no usable device and falls back to the CPU, where an
// SDXL render takes minutes per step. A rig that serves from the CPU is a rig
// paying GPU rates for nothing, and it looks entirely healthy from outside.
func (s SystemStats) HasCUDA() bool {
	for _, d := range s.Devices {
		if d.Type == "cuda" && d.VRAMTotal > 0 {
			return true
		}
	}
	return false
}

func (c *Client) base() string {
	return "http://" + c.Addr
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.Probe {
		req.Header.Set(runtime.ProbeHeader, "1")
	}
	cl := c.HTTP
	if cl == nil {
		cl = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	// Generous, because /view returns an image and a large batch render is
	// several tens of megabytes.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return raw, resp.StatusCode, nil
}

// Stats asks what hardware ComfyUI found.
func (c *Client) Stats(ctx context.Context) (*SystemStats, error) {
	raw, code, err := c.do(ctx, http.MethodGet, "/system_stats", nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, errs.Newf(errs.ClassHostFailure, "comfy.Stats", "http %d", code)
	}
	var s SystemStats
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, errs.Newf(errs.ClassHostFailure, "comfy.Stats", "decode: %v", err)
	}
	return &s, nil
}

// Submit enqueues a graph and returns its prompt id.
//
// A rejected graph is a model-class failure rather than a host-class one: the
// node errors come from the graph's own contents, so the next machine rejects
// it identically and falling back to one would buy a second rental to be told
// the same thing (FR-PROV-05).
func (c *Client) Submit(ctx context.Context, graph json.RawMessage, clientID string) (*PromptResult, error) {
	body, err := json.Marshal(struct {
		Prompt   json.RawMessage `json:"prompt"`
		ClientID string          `json:"client_id"`
	}{Prompt: graph, ClientID: clientID})
	if err != nil {
		return nil, err
	}
	raw, code, err := c.do(ctx, http.MethodPost, "/prompt", body)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, errs.Newf(errs.ClassModelFailure, "comfy.Submit",
			"graph rejected: http %d: %s", code, firstLine(raw))
	}
	var res PromptResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, errs.Newf(errs.ClassHostFailure, "comfy.Submit", "decode: %v", err)
	}
	if res.PromptID == "" {
		return nil, errs.Newf(errs.ClassModelFailure, "comfy.Submit",
			"no prompt id in the reply: %s", firstLine(raw))
	}
	if len(res.NodeErrors) > 0 {
		return nil, errs.Newf(errs.ClassModelFailure, "comfy.Submit",
			"graph has node errors: %s", firstLine(raw))
	}
	return &res, nil
}

// History returns what became of one submitted graph, and whether ComfyUI has
// heard of it yet.
func (c *Client) History(ctx context.Context, promptID string) (*HistoryEntry, bool, error) {
	raw, code, err := c.do(ctx, http.MethodGet, "/history/"+url.PathEscape(promptID), nil)
	if err != nil {
		return nil, false, err
	}
	if code != http.StatusOK {
		return nil, false, errs.Newf(errs.ClassHostFailure, "comfy.History", "http %d", code)
	}
	var all map[string]HistoryEntry
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, false, errs.Newf(errs.ClassHostFailure, "comfy.History", "decode: %v", err)
	}
	e, ok := all[promptID]
	if !ok {
		return nil, false, nil
	}
	return &e, true, nil
}

// Fetch downloads one artefact.
func (c *Client) Fetch(ctx context.Context, f OutputFile) ([]byte, error) {
	q := url.Values{}
	q.Set("filename", f.Filename)
	q.Set("subfolder", f.Subfolder)
	kind := f.Type
	if kind == "" {
		kind = "output"
	}
	q.Set("type", kind)
	raw, code, err := c.do(ctx, http.MethodGet, "/view?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, errs.Newf(errs.ClassHostFailure, "comfy.Fetch",
			"%s: http %d", f.Filename, code)
	}
	if len(raw) == 0 {
		return nil, errs.Newf(errs.ClassHostFailure, "comfy.Fetch",
			"%s: empty response", f.Filename)
	}
	return raw, nil
}

// Await polls until a graph finishes, and returns what it produced.
//
// Polling rather than the WebSocket, and the choice is deliberate. /ws carries
// per-step progress and is what the browser uses, but a supervisor needs a
// decision — finished, failed, or still going — and /history answers exactly
// that with no connection to hold open across a rig replacement. The browser
// keeps its WebSocket; LARRI does not need one.
func (c *Client) Await(ctx context.Context, promptID string, poll time.Duration) (*HistoryEntry, error) {
	if poll <= 0 {
		poll = 2 * time.Second
	}
	for {
		entry, known, err := c.History(ctx, promptID)
		if err == nil && known && entry.Status.Completed {
			if entry.Failed() {
				return entry, errs.Newf(errs.ClassModelFailure, "comfy.Await",
					"graph finished as %s", entry.Status.StatusStr)
			}
			return entry, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// Reachable reports whether anything is answering, without judging what.
func (c *Client) Reachable(ctx context.Context) error {
	_, code, err := c.do(ctx, http.MethodGet, "/system_stats", nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return errs.Newf(errs.ClassHostFailure, "comfy.Reachable", "http %d", code)
	}
	return nil
}

// LocalAddr renders a host and port for Client.Addr.
func LocalAddr(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func firstLine(b []byte) string {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	}
	if len(b) > 240 {
		b = b[:240]
	}
	return string(bytes.TrimSpace(b))
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
