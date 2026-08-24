// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package vastai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go.sovrenix.com/larri/internal/buildinfo"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/secret"
)

// DefaultBaseURL is Vast.ai's API host.
const DefaultBaseURL = "https://console.vast.ai"

// Endpoint paths.
//
// The version prefixes genuinely differ per endpoint, and that is not a
// mistake in this file: search and create are documented under v0 while the
// instance listing has moved to v1. Hard-coding one version for the whole
// adapter would have broken the listing silently, which is R-02 exactly. Each
// path is therefore stated separately and verified by the live contract test.
const (
	// Trailing slashes are not cosmetic: without them the API answers 301 to
	// the slashed form. Go follows the redirect, so the calls worked — but
	// every request paid for two round trips, and a redirect that ever crossed
	// hosts would have had the Authorization header stripped in flight.
	pathSearch  = "/api/v0/bundles/"
	pathCreate  = "/api/v0/asks/%s/"      // offer id
	pathDestroy = "/api/v0/instances/%s/" // instance id
	pathGet     = "/api/v0/instances/%s/" // instance id, single-instance read
	pathAttach  = "/api/v0/instances/%s/ssh/"
	pathList    = "/api/v1/instances/"
)

// listPageMax is the provider's ceiling, not our choice: the documented limit
// is "default 25, max 25". Asking for more does not raise it.
const listPageMax = 25

// searchLimit caps offers requested per search.
//
// A live probe walked the limit upward and found no server-side ceiling: 100,
// 500 and 1000 each returned exactly what was asked for, while 5000 returned
// 2382 — the entire rentable on-demand market. The cap was ours alone.
//
// That matters because the server sorts by price ascending. At the previous
// limit of 500, LARRI saw the 500 cheapest of 2382 offers and was blind to
// four fifths of the market, entirely at the more expensive end — which is
// where the better-fitting cards live, and fit exists precisely to stop the
// operator over-paying for VRAM they cannot use (§8).
//
// The value now sits well clear of the observed market size, and Search still
// reports a full page, because the market will grow and the notice is what
// keeps that growth from silently reintroducing the blind spot.
const searchLimit = 10000

// listPageBudget bounds pagination so a paging bug cannot spin forever. 400
// pages is 10,000 instances — far past any plausible account, and cheap
// insurance against a next_token that never clears.
const listPageBudget = 400

// Client is a thin, strict HTTP client for the Vast.ai API.
type Client struct {
	BaseURL string
	Key     secret.Secret
	HTTP    *http.Client
}

// NewClient builds a client with sane timeouts.
func NewClient(key secret.Secret) *Client {
	return &Client{
		BaseURL: DefaultBaseURL,
		Key:     key,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// do issues one request and decodes the response strictly.
//
// Strictness is the point (R-02). A provider that renames dph_total or changes
// gpu_ram's unit must produce a loud failure here, because the alternative is
// a mis-parsed price or VRAM figure that spends money on a wrong assumption.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("vastai: marshal request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	url := strings.TrimRight(c.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return fmt.Errorf("vastai: build request: %w", err)
	}
	// FR-SEC-10: the key travels in a header, never a query string, where
	// every intermediary's access log would capture it.
	req.Header.Set("Authorization", "Bearer "+c.Key.Reveal())
	req.Header.Set("Accept", "application/json")
	// Providers log this, and a version in their log is what turns "some
	// client is hammering the offers endpoint" into a specific release with a
	// specific bug — including for them, when they need to tell us.
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// A transport error on a mutation is an unknown outcome, not a
		// failure: the request may have been executed. The caller decides,
		// and must reconcile rather than retry (R-07).
		return errs.New(classifyTransport(method), "vastai."+method, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return errs.New(classifyTransport(method), "vastai."+method, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpError(method, resp.StatusCode, c.redactBody(raw))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errs.Newf(errs.ClassProviderTransient, "vastai."+method,
			"decode response: %v", err)
	}
	return nil
}

// A note on decoding strictness, because the obvious approach is wrong here.
//
// The instinct for R-02 is DisallowUnknownFields, so a changed payload fails
// loudly. Against this API it fails uselessly: a Vast offer carries well over
// a hundred fields and LARRI depends on about fifteen, so every single response
// would be flagged and the signal would be pure noise inside a week.
//
// The hazard is not a field being ADDED — that is routine and harmless. It is
// a field LARRI depends on being renamed, removed, or changed in unit, because
// a mis-parsed price or VRAM figure spends money on a wrong assumption.
//
// So strictness lives in normalise(): fields we rely on are decoded as
// pointers, absence is distinguishable from zero, and a missing one is a loud
// error naming the field. Fields we ignore stay ignored.

// ShapeDrift reports a payload that decoded but failed validation — a field
// LARRI depends on was absent or implausible.
type ShapeDrift struct {
	Op  string
	Err error
}

func (d *ShapeDrift) Error() string {
	return fmt.Sprintf("vastai: %s: %v", d.Op, d.Err)
}

func classifyTransport(method string) errs.Class {
	switch method {
	case http.MethodPut, http.MethodPost, http.MethodDelete:
		return errs.ClassProviderUnknownOutcome
	default:
		return errs.ClassProviderTransient
	}
}

// secretPattern matches credential-shaped JSON fields in a provider's error
// body.
//
// Vast echoes the caller's own API key back inside a 400 response — the whole
// authenticated context, api_key included. That body went verbatim into an
// error, and errors here are logged, journalled and returned to MCP agents, so
// a malformed request id was enough to write the account key into places it
// must never be (FR-SEC-02).
//
// Redacting by pattern as well as by exact value, because the exact value only
// covers the key LARRI holds: a provider that echoed a *different* secret back
// would otherwise sail through.
var secretPattern = regexp.MustCompile(
	`(?i)(['"]?(?:api_?key|apikey|token|password|secret|authorization)['"]?\s*[:=]\s*)` +
		// An optional auth scheme, so "Bearer sk-…" redacts the part that
		// matters and keeps the part that says what kind of credential it was.
		`(['"]?(?:Bearer\s+|Basic\s+|Token\s+)?)([A-Za-z0-9._\-]{8,})(['"]?)`)

// redactBody removes credentials from a provider's error body before it can
// reach a log, a journal entry, or an agent.
func (c *Client) redactBody(body []byte) string {
	msg := strings.TrimSpace(string(body))
	// The key we hold, by exact match: the surest removal there is.
	if k := c.Key.Reveal(); len(k) >= 8 {
		msg = strings.ReplaceAll(msg, k, "***")
	}
	msg = secretPattern.ReplaceAllString(msg, "${1}${2}***${4}")
	if len(msg) > 400 {
		msg = msg[:400]
	}
	return msg
}

func httpError(method string, code int, msg string) error {
	switch {
	case code == http.StatusTooManyRequests, code >= 500:
		return errs.Newf(errs.ClassProviderTransient, "vastai."+method,
			"http %d: %s", code, msg)
	case code == http.StatusUnauthorized, code == http.StatusForbidden:
		return errs.Newf(errs.ClassModelFailure, "vastai."+method,
			"http %d: check VASTAI_API_KEY: %s", code, msg)
	default:
		return errs.Newf(errs.ClassModelFailure, "vastai."+method,
			"http %d: %s", code, msg)
	}
}
