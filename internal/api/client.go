package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"

	"golang.org/x/oauth2"

	"github.com/infracost/lsp/internal/trace"
)

// TokenSource is a thread-safe, mutable wrapper around an oauth2.TokenSource.
// It allows the underlying source to be swapped (e.g. after login) without
// rebuilding HTTP clients that depend on it.
type TokenSource struct {
	mu sync.RWMutex
	ts oauth2.TokenSource
}

// NewTokenSource creates a TokenSource wrapping ts, which may be nil.
func NewTokenSource(ts oauth2.TokenSource) *TokenSource {
	return &TokenSource{ts: ts}
}

// Set replaces the underlying token source.
func (s *TokenSource) Set(ts oauth2.TokenSource) {
	s.mu.Lock()
	s.ts = ts
	s.mu.Unlock()
}

// Token returns a token from the underlying source.
func (s *TokenSource) Token() (*oauth2.Token, error) {
	s.mu.RLock()
	ts := s.ts
	s.mu.RUnlock()
	if ts == nil {
		return nil, fmt.Errorf("no token source configured")
	}
	return ts.Token()
}

// Valid reports whether a token source is configured.
func (s *TokenSource) Valid() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ts != nil
}

// Transport is an http.RoundTripper that adds User-Agent, trace, and
// x-infracost-org-id headers to outgoing requests. Authorization is handled by
// the wrapped OAuth transport.
type Transport struct {
	Base  http.RoundTripper
	orgID atomic.Pointer[string]
}

// SetOrgID updates the organization ID sent with subsequent requests.
func (t *Transport) SetOrgID(id string) {
	t.orgID.Store(&id)
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("User-Agent", trace.UserAgent)
	r.Header.Set("X-Infracost-Trace-ID", trace.ID)
	if id := t.orgID.Load(); id != nil && *id != "" {
		r.Header.Set("x-infracost-org-id", *id)
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// NewHTTPClient creates an *http.Client whose transport uses oauth2 for auth
// and adds User-Agent, trace, and x-infracost-org-id headers. Returns both the
// client and its Transport so callers can update the org ID later via SetOrgID.
func NewHTTPClient(ts *TokenSource) (*http.Client, *Transport) {
	oauthClient := oauth2.NewClient(context.Background(), ts)
	t := &Transport{
		Base: oauthClient.Transport,
	}
	return &http.Client{Transport: t}, t
}
