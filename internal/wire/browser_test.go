// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package wire

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/secret"
)

// browserProxy stands a proxy in front of a stub upstream and returns a client
// that does not follow redirects, so the session exchange can be inspected.
func browserProxy(t *testing.T) (*Proxy, *http.Client, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>comfyui</html>"))
	}))
	t.Cleanup(up.Close)

	p, err := NewProxy(0)
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := strings.Cut(strings.TrimPrefix(up.URL, "http://"), ":")
	var n int
	for _, c := range port {
		n = n*10 + int(c-'0')
	}
	p.SetUpstream(Upstream{Host: host, Port: n})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = p.Serve(ctx) }()
	t.Cleanup(func() { _ = p.Close() })

	cl := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	// Give the listener a moment to accept.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := cl.Get(p.addr()); err == nil {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	return p, cl, up
}

func (p *Proxy) addr() string { return "http://127.0.0.1:" + itoa(p.LocalPort()) }

// A browser cannot send an Authorization header on a navigation or a WebSocket
// handshake, so a surface the operator opens needs a credential it will
// actually send. The local key stays mandatory either way (FR-SEC-09).
func TestBrowserSessionExchangesATokenForACookie(t *testing.T) {
	p, cl, _ := browserProxy(t)
	tok := secret.New("browser-token-value")
	p.EnableBrowserSession(tok)

	// Without any credential, nothing gets through.
	resp, err := cl.Get(p.addr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request got %d, want 401", resp.StatusCode)
	}

	// The one-time URL sets the cookie and redirects the token out of the bar.
	resp, err = cl.Get(p.NewSessionURL())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("session exchange got %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Errorf("redirected to %q; the token must not survive in the url", loc)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie was set")
	}
	if !cookie.HttpOnly {
		t.Error("the session cookie is readable by script in a page served " +
			"from a host with root")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Error("the session cookie is not SameSite=Strict")
	}

	// And with the cookie, the proxied page comes through.
	req, _ := http.NewRequest(http.MethodGet, p.addr()+"/", nil)
	req.AddCookie(cookie)
	resp, err = cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cookie-authenticated request got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Error("the rig's ui can be framed by another page and driven invisibly")
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("no nosniff on a page whose content comes from an untrusted host")
	}
}

func TestAWrongSessionTokenIsRefused(t *testing.T) {
	p, cl, _ := browserProxy(t)
	p.EnableBrowserSession(secret.New("right"))
	resp, err := cl.Get(p.addr() + SessionPath + "?t=wrong")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong token got %d, want 401", resp.StatusCode)
	}
}

// A page on the open web must not be able to drive a rig the operator is
// paying for, whatever credential a browser was persuaded to attach.
func TestCrossOriginRequestsAreRefused(t *testing.T) {
	p, cl, _ := browserProxy(t)
	tok := secret.New("browser-token-value")
	p.EnableBrowserSession(tok)

	req, _ := http.NewRequest(http.MethodPost, p.addr()+"/prompt", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: tok.Reveal()})
	req.Header.Set("Origin", "https://evil.example")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a cross-origin request got %d, want 403", resp.StatusCode)
	}
}

func TestLoopbackOriginsAreAllowed(t *testing.T) {
	for _, origin := range []string{
		"", "http://127.0.0.1:8188", "http://localhost:8188", "http://[::1]:8188",
	} {
		if !validOrigin(origin) {
			t.Errorf("origin %q was refused", origin)
		}
	}
	for _, origin := range []string{
		"https://evil.example", "http://192.168.1.10", "null",
	} {
		if validOrigin(origin) {
			t.Errorf("origin %q was allowed", origin)
		}
	}
}

// Idle reclamation destroys. A browser tab polling its queue for hours is not
// the operator working, and counting it would make the timeout decorative.
func TestBackgroundTrafficDoesNotResetTheIdleClock(t *testing.T) {
	p, cl, _ := browserProxy(t)
	tok := secret.New("browser-token-value")
	p.EnableBrowserSession(tok)
	p.CountsAsWork = func(r *http.Request) bool {
		return r.Method == http.MethodPost && r.URL.Path == "/prompt"
	}

	get := func(method, path string) {
		req, _ := http.NewRequest(method, p.addr()+path, nil)
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: tok.Reveal()})
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	get(http.MethodGet, "/queue")
	get(http.MethodGet, "/history/p1")
	if p.Activity.Requests() != 0 {
		t.Errorf("chatter counted as work: requests = %d", p.Activity.Requests())
	}
	if p.Activity.Background() != 2 {
		t.Errorf("background = %d, want 2 — an exclusion nobody can see "+
			"is one nobody can check", p.Activity.Background())
	}
	if !p.Activity.LastOperatorRequest().IsZero() {
		t.Error("the idle clock was reset by background traffic")
	}

	get(http.MethodPost, "/prompt")
	if p.Activity.Requests() != 1 {
		t.Errorf("queueing a render did not count as work: %d", p.Activity.Requests())
	}
	if p.Activity.LastOperatorRequest().IsZero() {
		t.Error("the idle clock was not reset by real work")
	}
}

// With no rule supplied, every non-probe request is work. That is the /v1
// behaviour and it must not change underneath the engines.
func TestWithoutARuleEveryRequestIsStillWork(t *testing.T) {
	p, cl, _ := browserProxy(t)
	tok, _ := secret.Generate(16)
	p.AddClient("test", tok)

	req, _ := http.NewRequest(http.MethodGet, p.addr()+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Reveal())
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if p.Activity.Requests() != 1 {
		t.Errorf("requests = %d, want 1", p.Activity.Requests())
	}
	if p.Activity.Background() != 0 {
		t.Errorf("background = %d, want 0", p.Activity.Background())
	}
}

// Probe traffic is still excluded when a work rule is in force: the two
// exclusions are independent and both must hold.
func TestProbesStayExcludedAlongsideAWorkRule(t *testing.T) {
	p, cl, _ := browserProxy(t)
	tok, _ := secret.Generate(16)
	p.AddClient("test", tok)
	p.CountsAsWork = func(*http.Request) bool { return true }

	req, _ := http.NewRequest(http.MethodGet, p.addr()+"/system_stats", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Reveal())
	req.Header.Set(ProbeHeader, "1")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if p.Activity.Probes() != 1 {
		t.Errorf("probes = %d, want 1", p.Activity.Probes())
	}
	if p.Activity.Requests() != 0 {
		t.Error("a probe reset the idle clock it is meant to be excluded from")
	}
}

// Cookie auth exists only where it was switched on. A /v1 endpoint has no
// business accepting one.
func TestCookieAuthIsOffUnlessEnabled(t *testing.T) {
	p, cl, _ := browserProxy(t)
	req, _ := http.NewRequest(http.MethodGet, p.addr()+"/", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: "anything"})
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a cookie authenticated against a proxy with no browser session: %d",
			resp.StatusCode)
	}
	if p.NewSessionURL() != "" {
		t.Error("a session url exists without a browser session")
	}
}

// The link is one-time in fact and not only in name.
//
// It was named that in four places and was nothing of the sort: the URL token
// and the cookie were one value, so the exchange could not clear it without
// breaking cookie auth, and a printed link stayed live for the whole rig and
// minted a fresh cookie on every replay. These links are pasted into
// terminals, screen shares and issue threads — the redirect kept them out of
// the address bar and nothing kept them out of anywhere else.
func TestASpentSessionLinkCannotBeReplayed(t *testing.T) {
	p, cl, _ := browserProxy(t)
	p.EnableBrowserSession(secret.New("cookie-secret"))

	link := p.NewSessionURL()
	resp, err := cl.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first use got %d, want 303", resp.StatusCode)
	}

	// The same link again, which is what an attacker reading a scrollback has.
	again, err := cl.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	again.Body.Close()
	if again.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a spent link was accepted again with %d: it is a reusable "+
			"bearer credential, not a one-time link", again.StatusCode)
	}
	for _, c := range again.Cookies() {
		if c.Name == SessionCookie {
			t.Error("a replayed link minted another cookie")
		}
	}
}

// The URL token and the cookie must be different secrets, which is what makes
// spending one possible without revoking the other.
func TestTheLinkTokenIsNotTheCookie(t *testing.T) {
	p, cl, _ := browserProxy(t)
	cookieSecret := "cookie-secret"
	p.EnableBrowserSession(secret.New(cookieSecret))

	link := p.NewSessionURL()
	if strings.Contains(link, cookieSecret) {
		t.Fatalf("the link carries the cookie secret: %s", link)
	}
	resp, err := cl.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookie && c.Value == cookieSecret {
			return // the cookie is its own secret, and the link was another
		}
	}
	t.Error("the exchange did not issue the cookie secret")
}

// A second browser needs a second link, and issuing one must retire whatever
// was outstanding — so at most one live token exists at a time.
func TestReissuingRetiresTheUnusedLink(t *testing.T) {
	p, cl, _ := browserProxy(t)
	p.EnableBrowserSession(secret.New("cookie-secret"))

	first := p.NewSessionURL()
	second := p.NewSessionURL()
	if first == second {
		t.Fatal("reissuing returned the same link, so nothing was minted")
	}

	stale, err := cl.Get(first)
	if err != nil {
		t.Fatal(err)
	}
	stale.Body.Close()
	if stale.StatusCode != http.StatusUnauthorized {
		t.Errorf("the superseded link still works (%d): two live tokens exist",
			stale.StatusCode)
	}
	fresh, err := cl.Get(second)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Body.Close()
	if fresh.StatusCode != http.StatusSeeOther {
		t.Errorf("the freshly issued link got %d, want 303", fresh.StatusCode)
	}
}

// The cookie has to survive a real browser round trip over the loopback HTTP
// listener, which is the only place it is ever used.
//
// Everything else in this file inspects the Set-Cookie header directly, so
// nothing exercised a client actually sending it back — and an automated
// CodeQL fix duly marked the cookie Secure, which stops a client transmitting
// it over http. The exchange still redirected, the follow-up request arrived
// unauthenticated, and the whole suite stayed green because no test had a jar.
//
// LARRI has no TLS to offer here by design: the listener binds loopback and
// the SSH tunnel is the confidentiality boundary (§8). So the cookie must work
// over http, and this asserts it end to end rather than by reading a flag.
func TestTheSessionCookieSurvivesTheLoopbackRoundTrip(t *testing.T) {
	p, _, _ := browserProxy(t)
	p.EnableBrowserSession(secret.New("cookie-secret"))

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Follows redirects and keeps cookies, which is what a browser does and
	// what the rest of this file deliberately does not.
	cl := &http.Client{Timeout: 5 * time.Second, Jar: jar}

	resp, err := cl.Get(p.NewSessionURL())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("following the session link ended at %d, want 200: the cookie "+
			"was not sent back over the loopback listener", resp.StatusCode)
	}

	// And it keeps working, because a browser makes many requests per page.
	again, err := cl.Get(p.addr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer again.Body.Close()
	if again.StatusCode != http.StatusOK {
		t.Errorf("a later request got %d: the session did not persist", again.StatusCode)
	}
}
