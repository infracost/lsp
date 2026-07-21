package proxy

import (
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/oauth2"
)

func TestApply(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	Apply(Settings{
		HTTPProxy:  "http://proxy.example.com:8080",
		HTTPSProxy: "https://proxy.example.com:8443",
		NoProxy:    "localhost,127.0.0.1",
	})

	assert.Equal(t, "http://proxy.example.com:8080", os.Getenv("HTTP_PROXY"))
	assert.Equal(t, "https://proxy.example.com:8443", os.Getenv("HTTPS_PROXY"))
	assert.Equal(t, "localhost,127.0.0.1", os.Getenv("NO_PROXY"))
}

func TestApplyPreservesExistingEnv(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://env-proxy.example.com:8080")
	t.Setenv("http_proxy", "")

	Apply(Settings{HTTPProxy: "http://editor-proxy.example.com:8080"})

	assert.Equal(t, "http://env-proxy.example.com:8080", os.Getenv("HTTP_PROXY"))
}

func TestOAuthContext(t *testing.T) {
	ctx := OAuthContext(t.Context())
	client, ok := ctx.Value(oauth2.HTTPClient).(*http.Client)

	assert.True(t, ok)
	assert.Same(t, http.DefaultTransport, client.Transport)
}
