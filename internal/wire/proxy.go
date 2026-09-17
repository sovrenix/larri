// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package wire owns the hop from a fixed local port to whichever host is
// currently serving (P3).
//
// Clients are configured once against a stable loopback address and never
// learn that the machine behind it changed. The proxy is what makes that
// possible, and it is a component rather than a convenience: a bare port
// forward could not hold the port during instance replacement, could not
// enforce a local credential, could not count requests for the idle timer, and
// could not act as the credential boundary in §15.5.3.
package wire

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.sovrenix.com/larri/internal/secret"
)

// ProbeHeader marks LARRI's own health checks.
//
// FR-SUP-08: probe traffic must not reset the idle clock. §12 runs a real
// completion every 30 s by design, and if that counted as activity the idle
// timer could never expire — a feature that appears to work and protects
// nothing.
const ProbeHeader = "X-Larri-Probe"

// Activity records what the data plane has seen.
type Activity struct {
	lastOperator atomic.Int64 // unix nanos
	requests     atomic.Int64
	probes       atomic.Int64
	background   atomic.Int64
	inFlight     atomic.Int64
}

// LastOperatorRequest reports when an operator-attributable request last
// arrived. Zero if none has.
func (a *Activity) LastOperatorRequest() time.Time {
	n := a.lastOperator.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// Requests counts operator-attributable requests over the rig's life.
func (a *Activity) Requests() int64 { return a.requests.Load() }

// Probes counts LARRI's own health checks, kept separately so the exclusion is
// auditable rather than invisible.
func (a *Activity) Probes() int64 { return a.probes.Load() }

// Background counts requests that were carried but not treated as work — an
// open browser tab polling a queue it has nothing in. Counted rather than
// discarded for the same reason probes are: an exclusion nobody can see is an
// exclusion nobody can check, and this one decides when a rig is destroyed.
func (a *Activity) Background() int64 { return a.background.Load() }

// InFlight counts requests currently being served. A long generation is
// activity even though no new request has arrived.
func (a *Activity) InFlight() int64 { return a.inFlight.Load() }

// MarkOperator backdates the idle clock.
//
// Two callers need this and neither is the data plane, which sets the clock
// itself: a restarted LARRI restoring what it knew before the process died,
// and tests that must simulate an hour of silence without waiting an hour. It
// is not a way to keep a rig alive — writing a *future* time would defeat the
// timeout this exists to enforce — so it refuses to move the clock forward.
func (a *Activity) MarkOperator(t time.Time) {
	if t.After(time.Now()) {
		return
	}
	a.lastOperator.Store(t.UnixNano())
}

// EnterInFlight and ExitInFlight bracket a request the proxy is not carrying
// itself — a supervisor holding a rig alive across work it knows about, or a
// test proving a long generation is not idleness.
func (a *Activity) EnterInFlight() { a.inFlight.Add(1) }
func (a *Activity) ExitInFlight()  { a.inFlight.Add(-1) }

// IdleFor reports how long the rig has been without operator inference.
func (a *Activity) IdleFor(now time.Time) time.Duration {
	if a.InFlight() > 0 {
		return 0
	}
	last := a.LastOperatorRequest()
	if last.IsZero() {
		return 0
	}
	return now.Sub(last)
}

// Upstream is where the proxy forwards, and with which credential.
type Upstream struct {
	Host string
	Port int
	Key  secret.Secret
}

// Proxy is the local endpoint clients are wired against.
type Proxy struct {
	ln       net.Listener
	srv      *http.Server
	Activity Activity

	mu       sync.RWMutex
	upstream Upstream
	clients  map[string]string // sha256 of a token -> client name
	keys     KeySet

	// seen records which wired clients have actually sent a request.
	//
	// This is what makes verification possible in every tier (FR-WIRE-14),
	// and it is nearly free because the identity is already resolved to
	// authenticate the request and was previously discarded. Configuration
	// written correctly to an application that never re-read it is
	// indistinguishable from configuration that was never written, and the
	// operator finds out when their client fails rather than when LARRI does.
	// For a guided client it is the *only* evidence there is: nothing was
	// written, so there is no file to inspect.
	seen map[string]bool

	// browserToken is the cookie-shaped credential for a surface the
	// operator opens rather than configures. Empty disables that path
	// entirely, which is the default: a /v1 endpoint has no business
	// accepting a cookie.
	browserToken secret.Secret

	// CountsAsWork decides which requests reset the idle clock.
	//
	// Nil means every non-probe request does, which is right for /v1, where
	// a request is a completion and a completion is the work. It is wrong
	// for a surface with a browser attached to it: ComfyUI's frontend polls
	// its queue, reloads assets, and reconnects a WebSocket for as long as
	// the tab is open, so an idle timeout counting all of that would never
	// fire and the reclamation it implements would be decorative.
	//
	// Supplied by the caller rather than decided here, so wire keeps no
	// knowledge of any particular protocol's paths.
	CountsAsWork func(r *http.Request) bool
}

// KeySet is a store of client keys the proxy accepts beside the ones added to
// it directly — the keys operators configure clients with once, which outlive
// any one rig (invariant 3).
type KeySet interface {
	// Match reports the name of the key a presented value is, if any.
	Match(presented string) (name string, ok bool)
}

// NewProxy binds the local port. Binding here, before anything is declared
// healthy, is what makes a port already in use an error rather than a rig that
// reports READY while every client gets connection refused.
func NewProxy(localPort int) (*Proxy, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(localPort)))
	if err != nil {
		return nil, fmt.Errorf("wire: bind local port %d: %w", localPort, err)
	}
	return &Proxy{ln: ln, clients: map[string]string{}}, nil
}

// LocalPort reports the bound port.
func (p *Proxy) LocalPort() int {
	if a, ok := p.ln.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}

// SetUpstream points the proxy at a host. Called again on instance
// replacement, which is how the local port survives a rig being rebuilt
// underneath it (FR-WIRE-07).
func (p *Proxy) SetUpstream(u Upstream) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upstream = u
}

// AddClient registers a token for one wired client.
//
// Per-client rather than one shared secret (FR-SEC-23): a single client can be
// revoked without rewiring the others, a leaked config burns one credential,
// and requests carry an identity, so cost can be attributed per tool.
//
// Held as a hash, like a stored key, so the proxy keeps no copy of a value a
// client could present.
func (p *Proxy) AddClient(name string, token secret.Secret) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clients[hashToken(token.Reveal())] = name
}

// SetKeys adds a store of client keys to those the proxy accepts.
func (p *Proxy) SetKeys(k KeySet) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = k
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// markSeen records that a wired client has reached the endpoint.
func (p *Proxy) markSeen(client string) {
	if client == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen == nil {
		p.seen = make(map[string]bool)
	}
	p.seen[client] = true
}

// Seen reports whether a wired client has sent a request through the proxy.
//
// The answer is per rig rather than per session, because the proxy is what a
// rig replacement holds constant: a client that reached the old instance has
// demonstrably been configured, and demonstrating it again after a migration
// would ask the operator to do something they already did.
func (p *Proxy) Seen(client string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.seen[client]
}

// ProxyProber verifies wiring by asking the proxy whether the client showed up.
//
// The strongest verification available and the cheapest, which is an unusual
// combination worth taking. Checking that a file was written proves LARRI can
// write files; checking that the application it configures then authenticated
// with that client's own credential proves the thing the operator actually
// cares about. Per-client tokens (FR-SEC-23) are what make the attribution
// possible at all.
func ProxyProber(p *Proxy) Prober {
	if p == nil {
		return nil
	}
	return func(client string) (bool, error) { return p.Seen(client), nil }
}

// authenticate resolves a presented token to a client name.
func (p *Proxy) authenticate(header string) (string, bool) {
	tok := strings.TrimPrefix(header, "Bearer ")
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return "", false
	}
	h := []byte(hashToken(tok))
	p.mu.RLock()
	keys := p.keys
	name, ok := "", false
	for known, n := range p.clients {
		// Constant time, and over every entry, so neither a token nor its
		// position can be recovered from the timing.
		if subtle.ConstantTimeCompare(h, []byte(known)) == 1 {
			name, ok = n, true
		}
	}
	p.mu.RUnlock()
	if ok {
		return name, true
	}
	if keys != nil {
		return keys.Match(tok)
	}
	return "", false
}

// Serve runs the proxy until the context is cancelled.
func (p *Proxy) Serve(ctx context.Context) error {
	p.srv = &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.srv.Shutdown(sctx)
	}()
	err := p.srv.Serve(p.ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Close stops the proxy.
func (p *Proxy) Close() error { return p.ln.Close() }

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// FR-SEC-09: Host validation closes the DNS-rebinding path. A page in the
	// operator's browser can issue requests at a loopback port, and for LARRI
	// a request that fires is a request that spends.
	if !validHost(r.Host) {
		http.Error(w, "wire: unexpected Host header", http.StatusForbidden)
		return
	}
	// Checked before authentication, so a cross-origin request is refused
	// whatever credential a browser was persuaded to attach to it.
	if !validOrigin(r.Header.Get("Origin")) {
		http.Error(w, "wire: cross-origin request refused", http.StatusForbidden)
		return
	}
	if p.browserSessionEnabled() && r.URL.Path == SessionPath {
		p.serveSession(w, r)
		return
	}
	isProbe := r.Header.Get(ProbeHeader) != ""

	client, ok := p.authenticate(r.Header.Get("Authorization"))
	if !ok && p.cookieAuthenticated(r) {
		client, ok = "browser", true
	}
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "wire: missing or unknown API key", http.StatusUnauthorized)
		return
	}
	// Only the operator's own traffic counts as having reached the endpoint.
	// LARRI's probes carry the same credential and would otherwise verify the
	// wiring against itself, which is the same mistake as a health check that
	// resets the idle clock it enforces.
	if !isProbe {
		p.markSeen(client)
	}
	if p.browserSessionEnabled() {
		browserHeaders(w.Header())
	}

	p.mu.RLock()
	up := p.upstream
	p.mu.RUnlock()
	if up.Host == "" {
		// The rig is being replaced. Clients see an honest 503 rather than a
		// moved port, which is the whole point of holding the listener.
		http.Error(w, "wire: no rig is currently serving", http.StatusServiceUnavailable)
		return
	}

	switch {
	case isProbe:
		p.Activity.probes.Add(1)
	case p.CountsAsWork != nil && !p.CountsAsWork(r):
		// Carried, but not counted. A browser keeping a tab open is not an
		// operator using the rig, and treating it as one would hold a GPU
		// overnight on the strength of a reconnecting WebSocket.
		p.Activity.background.Add(1)
	default:
		p.Activity.requests.Add(1)
		p.Activity.lastOperator.Store(time.Now().UnixNano())
	}
	p.Activity.inFlight.Add(1)
	defer p.Activity.inFlight.Add(-1)

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = net.JoinHostPort(up.Host, fmt.Sprint(up.Port))
			// The credential boundary (FR-SEC-22). Header pass-through is the
			// default in every reverse proxy implementation, and here it
			// would ship the token shared by all the operator's IDEs to
			// untrusted hardware. The client's credential is removed and the
			// rig's substituted; neither is ever visible to the other side.
			req.Header.Del("Authorization")
			req.Header.Del(ProbeHeader)
			if !up.Key.Empty() {
				req.Header.Set("Authorization", "Bearer "+up.Key.Reveal())
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "wire: upstream unreachable: "+err.Error(),
				http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// validHost accepts only loopback names, so a rebinding attack that resolves
// an attacker-controlled name to 127.0.0.1 arrives with the wrong Host.
func validHost(h string) bool {
	if h == "" {
		return false
	}
	host, _, err := net.SplitHostPort(h)
	if err != nil {
		host = h
	}
	switch strings.ToLower(host) {
	case "127.0.0.1", "localhost", "[::1]", "::1":
		return true
	}
	return false
}

var _ io.Closer = (*Proxy)(nil)
