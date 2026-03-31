package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestTransport_WithToken(t *testing.T) {
	ts := NewTokenSource(oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: "my-jwt-token",
		TokenType:   "Bearer",
	}))
	client, _ := NewHTTPClient(ts)

	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := client.Get(srv.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, "Bearer my-jwt-token", captured.Get("Authorization"))
	assert.Contains(t, captured.Get("User-Agent"), "infracost-lsp")
}

func TestTransport_WithoutToken(t *testing.T) {
	ts := NewTokenSource(nil)
	client, _ := NewHTTPClient(ts)

	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := client.Get(srv.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Empty(t, captured.Get("Authorization"))
	assert.Contains(t, captured.Get("User-Agent"), "infracost-lsp")
}

func TestTransport_OrgID(t *testing.T) {
	ts := NewTokenSource(nil)
	client, transport := NewHTTPClient(ts)
	transport.SetOrgID("org-123")

	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := client.Get(srv.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, "org-123", captured.Get("x-infracost-org-id"))
}

func TestTransport_EmptyOrgID(t *testing.T) {
	ts := NewTokenSource(nil)
	client, _ := NewHTTPClient(ts)

	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := client.Get(srv.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Empty(t, captured.Get("x-infracost-org-id"))
}

func TestTokenSource_Set(t *testing.T) {
	ts := NewTokenSource(nil)
	assert.False(t, ts.Valid())

	_, err := ts.Token()
	assert.Error(t, err)

	ts.Set(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "tok"}))
	assert.True(t, ts.Valid())

	tok, err := ts.Token()
	require.NoError(t, err)
	assert.Equal(t, "tok", tok.AccessToken)
}
