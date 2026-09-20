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

	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/secret"
)

// upstreamRecorder stands in for the rig, capturing what actually arrived.
type upstreamRecorder struct {
	mu     sync.Mutex
	auth   []string
	probe  []string
	cookie []string
	refer  []string
	srv    *httptest.Server
}

func newUpstream(t *testing.T) *upstreamRecorder {
	t.Helper()
	u := &upstreamRecorder{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.auth = append(u.auth, r.Header.Get("Authorization"))
		u.probe = append(u.probe, r.Header.Get(ProbeHeader))
		u.cookie = append(u.cookie, r.Header.Get("Cookie"))
		u.refer = append(u.refer, r.Header.Get("Referer"))
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

// The probe header is declared in two packages — wire, which strips and counts
// it, and runtime, which sets it — because neither should depend on the other.
// That is fine only while they agree, and this is what makes a disagreement a
// failed test rather than an idle timer that silently never fires.
func TestProbeHeaderMatchesTheRuntimeConstant(t *testing.T) {
	if ProbeHeader != runtime.ProbeHeader {
		t.Fatalf("wire says %q, runtime says %q: probe traffic would reset the idle clock",
			ProbeHeader, runtime.ProbeHeader)
	}
}

// staticKeys stands in for the stored client keys.
type staticKeys map[string]string // value -> name

func (k staticKeys) Match(v string) (string, bool) { n, ok := k[v]; return n, ok }

// Stored client keys are accepted on every rig, beside the keys the proxy
// holds itself — and are stripped at the boundary like any other, never
// reaching the host.
func TestStoredClientKeysAreAcceptedAndStripped(t *testing.T) {
	up := newUpstream(t)
	p, base := startProxy(t, up, "RIG")
	p.AddClient("larri-probe", secret.New("PROBE"))
	p.SetKeys(staticKeys{"STORED-KEY": "continue"})

	for tok, want := range map[string]int{"STORED-KEY": http.StatusOK, "PROBE": http.StatusOK, "OTHER": http.StatusUnauthorized} {
		r := post(t, base, tok, nil)
		r.Body.Close()
		if r.StatusCode != want {
			t.Errorf("key %s: status %d, want %d", tok, r.StatusCode, want)
		}
	}
	if got := up.lastAuth(); got != "Bearer RIG" {
		t.Errorf("upstream saw %q; a client key must never reach the host", got)
	}
}

// Verification in every tier (FR-WIRE-14) needs evidence that the application
// reached the endpoint, and for a guided client there is no file to inspect
// instead. The identity was already resolved to authenticate the request and
// was being discarded.
func TestTheProxyRecordsWhichClientsActuallyShowedUp(t *testing.T) {
	up := newUpstream(t)
	p, base := startProxy(t, up, "rig-key")
	p.AddClient("subtitle-edit", secret.New("tok-se"))
	p.AddClient("buzz", secret.New("tok-buzz"))

	probe := ProxyProber(p)

	for _, name := range []string{"subtitle-edit", "buzz"} {
		if ok, _ := probe(name); ok {
			t.Errorf("%s was reported as having arrived before it sent anything", name)
		}
	}

	resp := post(t, base, "tok-se", nil)
	resp.Body.Close()

	if ok, _ := probe("subtitle-edit"); !ok {
		t.Error("a client that sent a request was not recorded as having arrived")
	}
	if ok, _ := probe("buzz"); ok {
		t.Error("a client that sent nothing was reported as having arrived")
	}
}

// LARRI's own probes carry the client's credential, so counting them would
// verify the wiring against itself — the same mistake as a health check that
// resets the idle clock it enforces.
func TestLARRIsOwnProbesDoNotVerifyTheWiring(t *testing.T) {
	up := newUpstream(t)
	p, base := startProxy(t, up, "rig-key")
	p.AddClient("subtitle-edit", secret.New("tok-se"))

	resp := post(t, base, "tok-se", map[string]string{ProbeHeader: "1"})
	resp.Body.Close()

	if ok, _ := ProxyProber(p)("subtitle-edit"); ok {
		t.Error("a larri probe was accepted as the operator's client arriving")
	}
}

func TestA503DoesNotCountAsAClientArriving(t *testing.T) {
	p, base := startProxy(t, nil, "")
	p.AddClient("subtitle-edit", secret.New("tok-se"))

	resp := post(t, base, "tok-se", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if ok, _ := ProxyProber(p)("subtitle-edit"); ok {
		t.Error("a request refused before any upstream existed marked the client seen")
	}
}

func TestAProberForNoProxyIsNil(t *testing.T) {
	if ProxyProber(nil) != nil {
		t.Error("a nil proxy produced a prober that would claim something")
	}
}

// lastCookie is what the rented host was sent.
func (u *upstreamRecorder) lastCookie() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.cookie) == 0 {
		return ""
	}
	return u.cookie[len(u.cookie)-1]
}

func (u *upstreamRecorder) lastReferer() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.refer) == 0 {
		return ""
	}
	return u.refer[len(u.refer)-1]
}

// The credential boundary has two headers, not one.
//
// Authorization was stripped and substituted from the start; Cookie was
// forwarded verbatim, so a browser-authenticated request handed LARRI's own
// session credential to the rented host — which has root and reads whatever
// arrives. Stripping one and passing the other is not a boundary, and ComfyUI
// has no use for it either: it holds no server-side credential at all.
func TestTheSessionCookieNeverReachesTheHost(t *testing.T) {
	up := newUpstream(t)
	p, base := startProxy(t, up, "rig-key")
	tok := secret.New("cookie-secret")
	p.EnableBrowserSession(tok)

	req, err := http.NewRequest(http.MethodGet, base+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: tok.Reveal()})
	// The proxied application's own cookie, which must survive: deleting the
	// whole header to protect one value breaks somebody else's frontend.
	req.AddCookie(&http.Cookie{Name: "comfy_layout", Value: "wide"})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the cookie-authenticated request got %d", resp.StatusCode)
	}

	got := up.lastCookie()
	if strings.Contains(got, tok.Reveal()) {
		t.Errorf("the host was sent LARRI's session credential: %q", got)
	}
	if strings.Contains(got, SessionCookie) {
		t.Errorf("the host was sent the session cookie: %q", got)
	}
	if !strings.Contains(got, "comfy_layout=wide") {
		t.Errorf("the application's own cookie was dropped: %q", got)
	}
}

func TestRefererIsNotForwardedToTheHost(t *testing.T) {
	up := newUpstream(t)
	p, base := startProxy(t, up, "rig-key")
	tok := secret.New("cookie-secret")
	p.EnableBrowserSession(tok)

	req, err := http.NewRequest(http.MethodGet, base+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: tok.Reveal()})
	req.Header.Set("Referer", "http://127.0.0.1:8188"+SessionPath+"?t=one-time")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the cookie-authenticated request got %d", resp.StatusCode)
	}
	if got := up.lastReferer(); got != "" {
		t.Errorf("the host saw a referer: %q", got)
	}
}

// A request that is still open must not hold the rig alive unless it is work.
//
// Classifying the traffic as background was necessary and not sufficient: every
// request was bracketed in-flight regardless of classification, and IdleFor
// returns zero while anything is in flight. A ComfyUI frontend holds a
// WebSocket through this proxy for as long as the tab is on screen, so the
// handler never returned, in-flight never reached zero, and the rig could not
// go idle — which is precisely what the comment beside the classification
// claimed to prevent.
//
// Asserted while the request is open, because that is the whole bug: after it
// completes the counter is zero either way.
func TestAHeldBackgroundRequestDoesNotHoldTheRig(t *testing.T) {
	var (
		open    = make(chan struct{})
		release = make(chan struct{})
		once    sync.Once
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(open) })
		<-release // the socket a browser leaves open
	}))
	defer upstream.Close()
	defer close(release)

	p, base := startProxy(t, nil, "")
	u, _ := url.Parse(upstream.URL)
	port, _ := strconv.Atoi(u.Port())
	p.SetUpstream(Upstream{Host: u.Hostname(), Port: port})
	p.AddClient("cli", secret.New("tok"))
	p.CountsAsWork = func(r *http.Request) bool {
		return r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions"
	}
	p.Activity.MarkOperator(time.Now().Add(-time.Hour))

	go func() {
		req, err := http.NewRequest(http.MethodGet, base+"/ws", nil)
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer tok")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	<-open // the proxy is carrying it now, and will be until release

	if n := p.Activity.InFlight(); n != 0 {
		t.Errorf("in flight = %d while a background request is open", n)
	}
	if idle := p.Activity.IdleFor(time.Now()); idle < 59*time.Minute {
		t.Errorf("idle = %s while nothing but a held background request is open: "+
			"a tab left on screen would hold the rig forever", idle.Round(time.Second))
	}
}

// The same, for work: a long generation is activity even though no new request
// has arrived, and that must keep holding the rig.
func TestAHeldWorkRequestDoesHoldTheRig(t *testing.T) {
	var (
		open    = make(chan struct{})
		release = make(chan struct{})
		once    sync.Once
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(open) })
		<-release
	}))
	defer upstream.Close()
	defer close(release)

	p, base := startProxy(t, nil, "")
	u, _ := url.Parse(upstream.URL)
	port, _ := strconv.Atoi(u.Port())
	p.SetUpstream(Upstream{Host: u.Hostname(), Port: port})
	p.AddClient("cli", secret.New("tok"))
	p.Activity.MarkOperator(time.Now().Add(-time.Hour))

	go func() {
		resp := post(t, base, "tok", nil)
		resp.Body.Close()
	}()
	<-open

	if n := p.Activity.InFlight(); n != 1 {
		t.Errorf("in flight = %d while a completion is being generated", n)
	}
	if idle := p.Activity.IdleFor(time.Now()); idle != 0 {
		t.Errorf("idle = %s during a completion: a long generation is activity", idle)
	}
}
