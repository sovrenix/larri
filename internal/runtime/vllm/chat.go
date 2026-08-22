// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package vllm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"go.sovrenix.com/larri/internal/runtime"
)

// ChatRequest is one OpenAI-compatible completion call.
//
// Readiness checks whatever address it is given, and the orchestrator gives it
// the **local** end of the tunnel rather than the remote one. That is
// deliberate: a check run on the host would prove only that vLLM answers
// itself, while a check run through the tunnel proves the whole path — the
// forward is carrying traffic, the proxy is substituting the rig credential,
// and the model produces a token. READY should mean the thing the operator is
// about to do actually works.
type ChatRequest struct {
	Endpoint runtime.Endpoint
	Body     []byte
	Timeout  time.Duration
}

// ChatResponse is the subset of the reply readiness cares about.
type ChatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// Do issues the request.
func (r ChatRequest) Do(ctx context.Context) (*ChatResponse, error) {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	url := "http://" + net.JoinHostPort(r.Endpoint.Host, strconv.Itoa(r.Endpoint.Port)) +
		"/v1/chat/completions"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(r.Body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if !r.Endpoint.Key.Empty() {
		req.Header.Set("Authorization", "Bearer "+r.Endpoint.Key.Reveal())
	}

	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, firstLine(raw))
	}
	var out ChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode completion: %w", err)
	}
	return &out, nil
}

func firstLine(b []byte) string {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	}
	if len(b) > 200 {
		b = b[:200]
	}
	return string(bytes.TrimSpace(b))
}
