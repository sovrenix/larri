// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package wire

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"go.sovrenix.com/larri/internal/secret"
)

// SessionCookie is the name of the browser-facing credential.
const SessionCookie = "larri_session"

// SessionPath is where a browser spends a one-time token for that cookie.
const SessionPath = "/__larri/session"

// EnableBrowserSession lets a browser authenticate to this proxy.
//
// The local API key is mandatory and stays mandatory (FR-SEC-09): loopback is
// not a per-user boundary, any page in the operator's browser can fire
// requests at a loopback port, and for LARRI a request that fires is a request
// that spends. What a browser cannot do is present one. Navigation carries no
// Authorization header, a WebSocket handshake carries no Authorization header,
// and neither can be made to — so a surface an operator *opens* rather than
// *configures* needs a credential shaped like a cookie.
//
// The exchange is a one-time URL: LARRI prints a link carrying an exchange
// token, the browser follows it once, the token is traded for an HttpOnly
// cookie and then **cleared**, and the URL is redirected away so nothing
// lingers in history or in a Referer.
//
// The two secrets are separate, and an earlier version's were not. Sharing one
// value meant the cookie could not be issued without leaving the URL
// credential live, so a link described everywhere as one-time actually worked
// for the rig's whole life and minted a fresh cookie every time it was
// replayed. These links are pasted into terminals, screen shares and issue
// threads; the redirect kept them out of the address bar and nothing kept them
// out of anywhere else.
//
// This does not widen what can reach the rig. It narrows it: before this, a
// browser-facing workload would have needed the listener opened to
// unauthenticated traffic.
func (p *Proxy) EnableBrowserSession(token secret.Secret) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.browserToken = token
}

// browserSessionEnabled reports whether cookie auth is available.
func (p *Proxy) browserSessionEnabled() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return !p.browserToken.Empty()
}

// NewSessionURL mints a fresh one-time link and returns it.
//
// Minting rather than reading, because the previous one is gone: a link is
// spent when it is used, so an operator opening the rig in a second browser
// needs a second link. Issuing one invalidates any earlier link that has not
// been used, which is the property worth having — at most one live token
// exists at a time, and it is the one just printed.
//
// Empty when no browser session is enabled, which is every claw an operator
// configures rather than opens.
func (p *Proxy) NewSessionURL() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.browserToken.Empty() {
		return "", nil
	}
	tok, err := secret.Generate(32)
	if err != nil {
		// Nothing to fall back to: a link carrying a predictable token would
		// be worse than no link, and the operator can still reach the rig by
		// asking for another.
		return "", err
	}
	p.exchangeToken = tok
	return "http://127.0.0.1:" + itoa(p.LocalPort()) + SessionPath +
		"?t=" + url.QueryEscape(tok.Reveal()), nil
}

// serveSession trades the one-time token for a cookie and spends the token.
func (p *Proxy) serveSession(w http.ResponseWriter, r *http.Request) {
	presented := r.URL.Query().Get("t")

	p.mu.Lock()
	exch, cookie := p.exchangeToken, p.browserToken
	ok := !exch.Empty() && presented != "" && secret.New(presented).Equal(exch)
	if ok {
		// Spent, under the same lock that read it, so two requests racing the
		// same link cannot both be served.
		p.exchangeToken = secret.Secret{}
	}
	p.mu.Unlock()

	if !ok {
		http.Error(w, "wire: missing or unknown session token", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:  SessionCookie,
		Value: cookie.Reveal(),
		Path:  "/",
		// HttpOnly so script in the proxied page cannot read the credential
		// back out. The page is served from a host with root (§15.4), so its
		// contents are not assumed friendly.
		HttpOnly: true,
		// Secure is deliberately NOT set, and a scanner will ask for it.
		//
		// This listener is http on loopback and there is no TLS to offer it:
		// the SSH tunnel is the confidentiality boundary (§8), and the cookie
		// never leaves this machine. Secure stops a client sending the
		// credential over http at all, which is every request this cookie
		// exists for. Chrome and Firefox treat http://127.0.0.1 as a secure
		// context and would send it anyway; Safari and embedded webviews have
		// not always, and a Go cookie jar does not — so setting it trades a
		// finding nobody can exploit for a surface that silently 401s in some
		// browsers and not others.
		//
		// It has been set once already, by an automated fix, and the
		// end-to-end test that caught it was rewritten to assert the 401
		// rather than the round trip. If a scan asks again, the answer is
		// still no; suppress the finding rather than the test.
		// Strict so a cross-site request cannot carry it. Origin is checked
		// as well rather than instead: the two fail in different ways, and a
		// control that depends on one browser behaviour is one deprecation
		// away from not being a control.
		SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("Referrer-Policy", "no-referrer")
	// 303 and a bare path, so the token leaves the address bar, the history,
	// and any Referer the page later sends.
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// cookieAuthenticated reports whether a request carries the session cookie.
func (p *Proxy) cookieAuthenticated(r *http.Request) bool {
	p.mu.RLock()
	tok := p.browserToken
	p.mu.RUnlock()
	if tok.Empty() {
		return false
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	return secret.New(c.Value).Equal(tok)
}

// validOrigin reports whether a request may proceed, given the port this proxy
// is published on.
//
// Absent is allowed: a top-level navigation sends no Origin, which is exactly
// how the operator opens the page.
//
// Present must match this listener exactly — a loopback host *and* this port.
// Accepting any loopback origin was not enough, and the gap is one the web
// platform hands you: cookies are scoped to a host and ignore the port, and
// SameSite compares sites rather than origins, so a page served from any other
// port on this machine is same-site, gets the session cookie attached to a
// credentialed fetch, and passed a check that only asked whether the host was
// loopback. A dev server on :3000, or any local app with an embedded web view,
// could then queue work on a GPU the operator is paying for — which invariant 8
// calls a financial attack rather than a nuisance, and which the comment here
// previously claimed to prevent while only stopping the open web.
//
// Loopback is still compared by value rather than by string, so reaching the
// rig as localhost and as 127.0.0.1 both work: same port, same server, and the
// spelling is the operator's choice.
func validOrigin(origin string, port int) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if !loopbackHost(u.Hostname()) {
		return false
	}
	// An origin with no port means the scheme default, which is never this
	// listener: the local port is always explicit.
	return u.Port() == itoa(port)
}

// loopbackHost reports whether a hostname names this machine.
func loopbackHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

// browserHeaders are set on every proxied response.
//
// Deliberately short of a full content-security-policy. A CSP belongs on pages
// LARRI itself renders, where it knows what they load; this response is
// ComfyUI's own frontend, and a policy written blind against somebody else's
// application breaks it in ways that look like LARRI being broken. What is
// here holds regardless of what the page contains: no MIME sniffing, no
// referrer leaking a local URL outward, and no framing, so another page cannot
// embed the rig's UI and drive it invisibly.
func browserHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
