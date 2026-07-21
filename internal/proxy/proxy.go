package proxy

import (
	"context"
	"net/http"
	"os"

	"golang.org/x/oauth2"
)

// Settings contains proxy settings passed by editor clients.
type Settings struct {
	HTTPProxy  string `json:"httpProxy"`
	HTTPSProxy string `json:"httpsProxy"`
	NoProxy    string `json:"noProxy"`
}

// Apply configures the process environment for Go's default HTTP transport.
// Existing environment variables win so users can override editor-provided
// settings when launching the language server.
func Apply(settings Settings) {
	setEnvIfUnset("HTTP_PROXY", "http_proxy", settings.HTTPProxy)
	setEnvIfUnset("HTTPS_PROXY", "https_proxy", settings.HTTPSProxy)
	setEnvIfUnset("NO_PROXY", "no_proxy", settings.NoProxy)
}

// OAuthContext returns a context whose oauth2 requests use a client backed by
// the default proxy-aware transport. This ensures auth/device-flow requests see
// proxy settings that were applied after process start.
func OAuthContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, &http.Client{
		Transport: http.DefaultTransport,
	})
}

func setEnvIfUnset(name string, altName string, value string) {
	if value == "" || os.Getenv(name) != "" || os.Getenv(altName) != "" {
		return
	}
	_ = os.Setenv(name, value)
}
