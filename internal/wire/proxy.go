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

// InFlight counts requests currently being served. A long generation is
// activity even though no new request has arrived.
func (a *Activity) InFlight() int64 { return a.inFlight.Load() }

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
	clients  map[string]secret.Secret // token -> client name
}

// NewProxy binds the local port. Binding here, before anything is declared
// healthy, is what makes a port already in use an error rather than a rig that
// reports READY while every client gets connection refused.
func NewProxy(localPort int) (*Proxy, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(localPort)))
	if err != nil {
		return nil, fmt.Errorf("wire: bind local port %d: %w", localPort, err)
	}
	return &Proxy{ln: ln, clients: map[string]secret.Secret{}}, nil
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
func (p *Proxy) AddClient(name string, token secret.Secret) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clients[token.Reveal()] = secret.New(name)
}

// authenticate resolves a presented token to a client name.
func (p *Proxy) authenticate(header string) (string, bool) {
	tok := strings.TrimPrefix(header, "Bearer ")
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return "", false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for known, name := range p.clients {
		// Constant time, so a token cannot be recovered a byte at a time.
		if secret.New(known).Equal(secret.New(tok)) {
			return name.Reveal(), true
		}
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
	isProbe := r.Header.Get(ProbeHeader) != ""

	client, ok := p.authenticate(r.Header.Get("Authorization"))
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "wire: missing or unknown API key", http.StatusUnauthorized)
		return
	}
	_ = client

	p.mu.RLock()
	up := p.upstream
	p.mu.RUnlock()
	if up.Host == "" {
		// The rig is being replaced. Clients see an honest 503 rather than a
		// moved port, which is the whole point of holding the listener.
		http.Error(w, "wire: no rig is currently serving", http.StatusServiceUnavailable)
		return
	}

	if isProbe {
		p.Activity.probes.Add(1)
	} else {
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
