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
	u, err := p.NewSessionURL()
	if err != nil {
		t.Fatal(err)
	}
	resp, err = cl.Get(u)
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
	if !cookie.Secure {
		t.Error("the session cookie is not Secure")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Error("the session cookie is not SameSite=Strict")
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("redirect referrer policy = %q, want no-referrer", got)
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

func TestOnlyThisListenersOwnOriginIsAllowed(t *testing.T) {
	const port = 8188
	// Absent, because a top-level navigation sends none — that is how the
	// operator opens the page. Then this listener, however it is spelled.
	for _, origin := range []string{
		"", "http://127.0.0.1:8188", "http://localhost:8188", "http://[::1]:8188",
	} {
		if !validOrigin(origin, port) {
			t.Errorf("origin %q was refused", origin)
		}
	}
	for _, origin := range []string{
		"https://evil.example", "http://192.168.1.10", "null",
		// The one that matters, and the one a loopback-only check let
		// through. Cookies ignore ports and SameSite compares sites, so a
		// page on any other local port is same-site: the browser attaches the
		// session cookie to a credentialed fetch and the rig serves it. A dev
		// server, or any local app with an embedded web view, could queue
		// work on a GPU the operator is paying for (invariant 8).
		"http://127.0.0.1:3000", "http://localhost:5173", "http://[::1]:9999",
		// No port means the scheme default, which this listener never is.
		"http://127.0.0.1", "http://localhost",
	} {
		if validOrigin(origin, port) {
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
	u, err := p.NewSessionURL()
	if err != nil {
		t.Fatal(err)
	}
	if u != "" {
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

	link, err := p.NewSessionURL()
	if err != nil {
		t.Fatal(err)
	}
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

	link, err := p.NewSessionURL()
	if err != nil {
		t.Fatal(err)
	}
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

	first, err := p.NewSessionURL()
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.NewSessionURL()
	if err != nil {
		t.Fatal(err)
	}
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

// A plain HTTP client jar does not replay a Secure cookie, which is the flag
// CodeQL required here. The browser-session path is therefore tested by the
// exchange and by explicit cookie replay above, not by trusting an insecure
// transport to send it back automatically.
func TestAGoCookieJarDoesNotReplayTheSecureSessionCookieOverHTTP(t *testing.T) {
	p, _, _ := browserProxy(t)
	p.EnableBrowserSession(secret.New("cookie-secret"))

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Follows redirects and keeps cookies, which is what a browser does and
	// what the rest of this file deliberately does not.
	cl := &http.Client{Timeout: 5 * time.Second, Jar: jar}

	link, err := p.NewSessionURL()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := cl.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("following the session link ended at %d, want 401", resp.StatusCode)
	}

	// And it stays unusable to the jar for the same reason.
	again, err := cl.Get(p.addr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer again.Body.Close()
	if again.StatusCode != http.StatusUnauthorized {
		t.Errorf("the jar replayed a Secure cookie over http with %d", again.StatusCode)
	}
}

// The attack the loopback-only check allowed, driven through the listener
// rather than the helper: a page on another local port, holding a cookie the
// browser attached for it.
//
// Nothing here is forged. Cookies are scoped to a host and ignore the port, and
// SameSite compares sites rather than origins, so a page served from any other
// port on this machine is same-site and the browser sends the session cookie on
// a credentialed fetch without being asked twice. The only thing standing
// between a local dev server and a GPU the operator is paying for is this
// check, and while it asked merely "is the origin loopback" there was nothing
// standing there at all.
func TestAPageOnAnotherLocalPortCannotDriveTheRig(t *testing.T) {
	p, cl, _ := browserProxy(t)
	tok := secret.New("cookie-secret")
	p.EnableBrowserSession(tok)

	// The cookie the browser would hold, exactly as the exchange issues it.
	withCookie := func(origin string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, p.addr()+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: tok.Reveal()})
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	evil := withCookie("http://127.0.0.1:3000")
	evil.Body.Close()
	if evil.StatusCode != http.StatusForbidden {
		t.Errorf("a page on :3000 drove the rig with a valid cookie and got %d, want 403",
			evil.StatusCode)
	}

	// The rig's own page must still work, or the fix has broken the feature.
	own := withCookie("http://127.0.0.1:" + itoa(p.LocalPort()))
	own.Body.Close()
	if own.StatusCode != http.StatusOK {
		t.Errorf("the rig's own frontend got %d, want 200", own.StatusCode)
	}

	// And a top-level navigation, which carries no Origin at all.
	nav := withCookie("")
	nav.Body.Close()
	if nav.StatusCode != http.StatusOK {
		t.Errorf("a navigation got %d, want 200", nav.StatusCode)
	}
}
