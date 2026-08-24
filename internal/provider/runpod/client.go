// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package runpod is the RunPod provider adapter.
//
// # Two APIs, deliberately
//
// RunPod's REST v2 owns the pod lifecycle and carries no prices: a pod's
// costPerHr is known only once it exists, and there is no catalogue endpoint.
// Price is load-bearing here — selection ranks on it and every ceiling is
// denominated in it — so Search reads the GraphQL catalogue instead, which
// returns each GPU type with its on-demand and spot price and answers
// unauthenticated.
//
// So: **GraphQL to decide what to rent, REST to rent it.** Not a preference;
// neither API can do the other's half.
//
// # What a RunPod offer is
//
// Not a machine. Vast lists individual hosts, each with its own price,
// reliability and machine id, and renting one is renting *that* box. RunPod
// sells a GPU *type* — "NVIDIA GeForce RTX 4090" — and places the pod itself.
// An offer here is therefore a class of hardware, which is why it reports no
// MachineID and no reliability: there is no host to name and no host to score
// (§5.4).
package runpod

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

const (
	// DefaultRESTURL owns the pod lifecycle.
	DefaultRESTURL = "https://rest.runpod.io/v1"
	// DefaultGraphQLURL owns the catalogue. Deprecated by RunPod in favour of
	// REST, and kept because REST publishes no prices — the moment it does,
	// this goes.
	DefaultGraphQLURL = "https://api.runpod.io/graphql"
)

// Client talks to both of RunPod's APIs.
type Client struct {
	Key        secret.Secret
	RESTURL    string
	GraphQLURL string
	// LogsURL is a printf template taking the pod id and a tail count. Its
	// own field because RunPod serves logs from a different host than pods.
	LogsURL string
	HTTP    *http.Client
}

// NewClient builds a client.
func NewClient(key secret.Secret) *Client {
	return &Client{
		Key: key, RESTURL: DefaultRESTURL, GraphQLURL: DefaultGraphQLURL,
		HTTP: &http.Client{Timeout: 60 * time.Second},
	}
}

// secretPattern matches credential-shaped fields in an error body.
//
// The same protection the Vast adapter needed after a live run found that
// provider echoing the caller's api_key back inside a 400. Whether RunPod does
// the same is unknown and beside the point: an error body is untrusted text
// that ends up in logs, journal entries and MCP results, and the cost of
// assuming otherwise is an account key written into all three (FR-SEC-02).
var secretPattern = regexp.MustCompile(
	`(?i)(['"]?(?:api_?key|apikey|token|password|secret|authorization)['"]?\s*[:=]\s*)` +
		`(['"]?(?:Bearer\s+|Basic\s+|Token\s+)?)([A-Za-z0-9._\-]{8,})(['"]?)`)

func (c *Client) redactBody(body []byte) string {
	msg := strings.TrimSpace(string(body))
	if k := c.Key.Reveal(); len(k) >= 8 {
		msg = strings.ReplaceAll(msg, k, "***")
	}
	msg = secretPattern.ReplaceAllString(msg, "${1}${2}***${4}")
	if len(msg) > 400 {
		msg = msg[:400]
	}
	return msg
}

// rest issues a REST v2 request.
//
// The key is checked before the network is touched. RunPod's catalogue is
// public, so LARRI allows a provider with no key in order that `larri offers`
// can compare prices before anyone signs up — which means every call that
// *does* need one has to say so plainly rather than returning a 401 from a
// depth the operator cannot place.
func (c *Client) rest(ctx context.Context, method, path string, body, out any) error {
	if c.Key.Empty() {
		return errs.Newf(errs.ClassModelFailure, "runpod."+method,
			"set RUNPOD_API_KEY: renting needs a key, searching does not")
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return errs.Newf(errs.ClassModelFailure, "runpod."+method, "encode: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	base := c.RESTURL
	if base == "" {
		base = DefaultRESTURL
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key.Reveal())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	cl := c.HTTP
	if cl == nil {
		cl = http.DefaultClient
	}
	resp, err := cl.Do(req)
	if err != nil {
		return errs.Newf(mutationClass(method), "runpod."+method, "%v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return errs.Newf(errs.ClassProviderTransient, "runpod."+method, "read: %v", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpError(method, resp.StatusCode, c.redactBody(raw))
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errs.Newf(errs.ClassProviderTransient, "runpod."+method, "decode: %v", err)
	}
	return nil
}

// graphql issues a catalogue query.
func (c *Client) graphql(ctx context.Context, query string, out any) error {
	body, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		return err
	}
	base := c.GraphQLURL
	if base == "" {
		base = DefaultGraphQLURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	// The catalogue answers unauthenticated, and the key is sent only when
	// present so an account with negotiated pricing sees its own. A key that
	// is present but wrong turns a public query into a 401, which is the
	// correct outcome and worth stating: search failing on a bad key is how
	// an operator learns the key is bad before a rental depends on it.
	if !c.Key.Empty() {
		req.Header.Set("Authorization", "Bearer "+c.Key.Reveal())
	}
	cl := c.HTTP
	if cl == nil {
		cl = http.DefaultClient
	}
	resp, err := cl.Do(req)
	if err != nil {
		return errs.Newf(errs.ClassProviderTransient, "runpod.catalogue", "%v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return errs.Newf(errs.ClassProviderTransient, "runpod.catalogue", "read: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return httpError("catalogue", resp.StatusCode, c.redactBody(raw))
	}
	// GraphQL reports failure inside a 200, so the status is not the check.
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return errs.Newf(errs.ClassProviderTransient, "runpod.catalogue", "decode: %v", err)
	}
	if len(envelope.Errors) > 0 {
		return errs.Newf(errs.ClassProviderTransient, "runpod.catalogue",
			"%s", envelope.Errors[0].Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(envelope.Data, out)
}

func mutationClass(method string) errs.Class {
	switch method {
	case http.MethodPost, http.MethodPatch, http.MethodDelete, http.MethodPut:
		return errs.ClassProviderUnknownOutcome
	default:
		return errs.ClassProviderTransient
	}
}

func httpError(op string, code int, msg string) error {
	switch {
	case code == http.StatusTooManyRequests, code >= 500:
		return errs.Newf(errs.ClassProviderTransient, "runpod."+op, "http %d: %s", code, msg)
	case code == http.StatusUnauthorized, code == http.StatusForbidden:
		return errs.Newf(errs.ClassModelFailure, "runpod."+op,
			"http %d: check RUNPOD_API_KEY: %s", code, msg)
	default:
		return errs.Newf(errs.ClassModelFailure, "runpod."+op, "http %d: %s", code, msg)
	}
}

// isNotFound reports a provider saying the thing is not there.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	e := strings.ToLower(err.Error())
	return strings.Contains(e, "http 404") || strings.Contains(e, "not found")
}

var _ = fmt.Sprintf
