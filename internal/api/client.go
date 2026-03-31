package api

import (
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

// Transport is an http.RoundTripper that adds authentication, User-Agent,
// and x-infracost-org-id headers to outgoing requests. Authentication is
// best-effort: if no token is available the request proceeds without it.
type Transport struct {
	Base  http.RoundTripper
	ts    *TokenSource
	orgID atomic.Pointer[string]
}

// SetOrgID updates the organization ID sent with subsequent requests.
func (t *Transport) SetOrgID(id string) {
	t.orgID.Store(&id)
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	if tok, err := t.ts.Token(); err == nil {
		r.Header.Set("Authorization", tok.Type()+" "+tok.AccessToken)
	}
	r.Header.Set("User-Agent", trace.UserAgent)
	if id := t.orgID.Load(); id != nil && *id != "" {
		r.Header.Set("x-infracost-org-id", *id)
	}
	return t.Base.RoundTrip(r)
}

// NewHTTPClient creates an *http.Client whose transport adds auth,
// User-Agent, and x-infracost-org-id headers. Returns both the client and
// its Transport so callers can update the org ID later via SetOrgID.
func NewHTTPClient(ts *TokenSource) (*http.Client, *Transport) {
	t := &Transport{
		Base: http.DefaultTransport,
		ts:   ts,
	}
	return &http.Client{Transport: t}, t
}
