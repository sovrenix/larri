// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package wire

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/secret"
)

// upstreamRecorder stands in for the rig, capturing what actually arrived.
type upstreamRecorder struct {
	mu    sync.Mutex
	auth  []string
	probe []string
	srv   *httptest.Server
}

func newUpstream(t *testing.T) *upstreamRecorder {
	t.Helper()
	u := &upstreamRecorder{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.auth = append(u.auth, r.Header.Get("Authorization"))
		u.probe = append(u.probe, r.Header.Get(ProbeHeader))
		u.mu.Unlock()
		fmt.Fprint(w, `{"choices":[{"message":{"content":"pong"}}]}`)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstreamRecorder) lastAuth() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.auth) == 0 {
		return ""
	}
	return u.auth[len(u.auth)-1]
}

func startProxy(t *testing.T, up *upstreamRecorder, rigKey string) (*Proxy, string) {
	t.Helper()
	p, err := NewProxy(0)
	if err != nil {
		t.Fatal(err)
	}
	if up != nil {
		u, _ := url.Parse(up.srv.URL)
		port, _ := strconv.Atoi(u.Port())
		p.SetUpstream(Upstream{Host: u.Hostname(), Port: port, Key: secret.New(rigKey)})
	}
	ctx, cancel := context.WithCancel(context.Background())
	go p.Serve(ctx)
	t.Cleanup(func() { cancel(); p.Close() })

	base := "http://127.0.0.1:" + strconv.Itoa(p.LocalPort())
	// Wait for the listener to start serving.
	for i := 0; i < 50; i++ {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(p.LocalPort()), time.Second)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return p, base
}

func post(t *testing.T, base, token string, hdrs map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// FR-SEC-22, and the rule most likely to be got wrong: header pass-through is
// the default in every reverse proxy implementation, and here it would ship
// the token shared by all the operator's IDEs to untrusted hardware.
func TestClientCredentialNeverReachesTheHost(t *testing.T) {
	up := newUpstream(t)
	p, base := startProxy(t, up, "RIG-TOKEN")
	p.AddClient("continue.dev", secret.New("CLIENT-TOKEN"))

	resp := post(t, base, "CLIENT-TOKEN", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := up.lastAuth()
	if strings.Contains(got, "CLIENT-TOKEN") {
		t.Fatal("the client's credential was forwarded to the rig")
	}
	if got != "Bearer RIG-TOKEN" {
		t.Errorf("upstream auth = %q, want the rig's own credential", got)
	}
}

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	up := newUpstream(t)
	_, base := startProxy(t, up, "RIG-TOKEN")

	for _, tok := range []string{"", "wrong-token"} {
		resp := post(t, base, tok, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q got %d, want 401", tok, resp.StatusCode)
		}
	}
	if n := up.lastAuth(); n != "" {
		t.Error("an unauthenticated request must not reach the rig at all")
	}
}

// FR-SEC-09: a page in the operator's browser can fire requests at a loopback
// port, and for LARRI a request that fires is a request that spends.
func TestUnexpectedHostHeaderIsRejected(t *testing.T) {
	up := newUpstream(t)
	p, base := startProxy(t, up, "RIG")
	p.AddClient("c", secret.New("T"))

	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer T")
	req.Host = "evil.example.com" // resolves to 127.0.0.1 in a rebinding attack
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a rebinding Host", resp.StatusCode)
	}
}

// FR-SUP-08, and the bug that would have silently disabled idle reclamation:
// §12 runs a real completion every 30 s, so if probes counted as activity the
// timer could never expire.
func TestProbeTrafficDoesNotResetTheIdleClock(t *testing.T) {
	up := newUpstream(t)
	p, base := startProxy(t, up, "RIG")
	p.AddClient("c", secret.New("T"))

	// One real request, then a run of probes.
	post(t, base, "T", nil).Body.Close()
	operatorAt := p.Activity.LastOperatorRequest()
	if operatorAt.IsZero() {
		t.Fatal("an operator request should have been recorded")
	}
	time.Sleep(5 * time.Millisecond)
	for i := 0; i < 5; i++ {
		post(t, base, "T", map[string]string{ProbeHeader: "health"}).Body.Close()
	}
	if got := p.Activity.LastOperatorRequest(); !got.Equal(operatorAt) {
		t.Fatal("health probes moved the idle clock; the timer could never fire")
	}
	if p.Activity.Requests() != 1 {
		t.Errorf("operator requests = %d, want 1", p.Activity.Requests())
	}
	// Counted separately rather than discarded, so the exclusion is auditable.
	if p.Activity.Probes() != 5 {
		t.Errorf("probes = %d, want 5", p.Activity.Probes())
	}
	// The marker must not leak upstream.
	up.mu.Lock()
	last := up.probe[len(up.probe)-1]
	up.mu.Unlock()
	if last != "" {
		t.Errorf("probe marker was forwarded to the rig: %q", last)
	}
}

// Holding the port during replacement is why clients are configured once. A
// moved port would mean rewriting every config on every rebuild.
func TestPortIsHeldWhileNoRigIsServing(t *testing.T) {
	p, base := startProxy(t, nil, "")
	p.AddClient("c", secret.New("T"))

	resp := post(t, base, "T", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 while no rig serves", resp.StatusCode)
	}
	// The listener is still bound, which is what "connection refused, not a
	// moved port" means in practice.
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(p.LocalPort()))
	if err != nil {
		t.Fatal("the port must stay bound across a replacement")
	}
	c.Close()
}

func TestBindingATakenPortFails(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	taken := blocker.Addr().(*net.TCPAddr).Port
	if _, err := NewProxy(taken); err == nil {
		t.Fatal("binding an occupied port must fail rather than appear to succeed")
	}
}

// A long generation is activity, even though no new request has arrived.
func TestInFlightRequestCountsAsActivity(t *testing.T) {
	var a Activity
	a.lastOperator.Store(time.Now().Add(-time.Hour).UnixNano())
	if a.IdleFor(time.Now()) < 59*time.Minute {
		t.Fatal("precondition: should look idle")
	}
	a.inFlight.Add(1)
	if a.IdleFor(time.Now()) != 0 {
		t.Error("a request still streaming means the rig is in use")
	}
}

func TestPerClientTokensRevokeIndependently(t *testing.T) {
	up := newUpstream(t)
	p, base := startProxy(t, up, "RIG")
	p.AddClient("continue", secret.New("TOK-A"))
	p.AddClient("librechat", secret.New("TOK-B"))

	for _, tok := range []string{"TOK-A", "TOK-B"} {
		r := post(t, base, tok, nil)
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Errorf("token %s got %d", tok, r.StatusCode)
		}
	}
}
