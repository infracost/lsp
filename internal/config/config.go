package config

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/infracost/cli/pkg/auth"
	"github.com/infracost/cli/pkg/environment"
	cliplugins "github.com/infracost/cli/pkg/plugins"
	"golang.org/x/oauth2"
)

type Config struct {
	SlogLevel       slog.Level
	Currency        string
	PricingEndpoint string
	DebugUI         string

	TokenSource       oauth2.TokenSource
	DashboardEndpoint string
	// Plugins is a pointer because cliplugins.Config carries mutexes and a
	// sync.Once for plugin-manager state, so it must never be copied by value.
	Plugins *cliplugins.Config
}

func Load(ctx context.Context) Config {
	cfg := Config{
		SlogLevel:       slog.LevelInfo,
		Currency:        "USD",
		PricingEndpoint: "https://pricing.api.infracost.io",
	}

	if os.Getenv("INFRACOST_LOG_LEVEL") == "debug" {
		cfg.SlogLevel = slog.LevelDebug
	}
	if v := os.Getenv("INFRACOST_CLI_CURRENCY"); v != "" {
		cfg.Currency = v
	}
	if v := os.Getenv("INFRACOST_CLI_PRICING_ENDPOINT"); v != "" {
		cfg.PricingEndpoint = v
	}
	cfg.DebugUI = os.Getenv("INFRACOST_DEBUG_UI")

	cfg.DashboardEndpoint = "https://dashboard.api.infracost.io"
	if v := os.Getenv("INFRACOST_CLI_DASHBOARD_ENDPOINT"); v != "" {
		cfg.DashboardEndpoint = v
	}

	cfg.TokenSource = loadAuthToken(ctx)
	cfg.Plugins = loadPluginsConfig()

	return cfg
}

// TokenCachePath returns the path to the LSP's own token cache file,
// separate from the CLI's token.json to avoid write collisions.
func TokenCachePath() string {
	dir, err := os.UserConfigDir()
	if err == nil {
		return filepath.Join(dir, "infracost", "lsp-token.json")
	}

	dir, err = os.UserHomeDir()
	if err == nil {
		return filepath.Join(dir, ".infracost", "lsp-token.json")
	}

	return filepath.Join(".infracost", "lsp-token.json")
}

func loadAuthToken(ctx context.Context) oauth2.TokenSource {
	cfg := auth.Config{
		Environment: environment.Production,
	}
	cfg.TokenCachePath = TokenCachePath()
	cfg.Process()
	cfg.UseAccessTokenCache = true
	tokenSource, _, err := cfg.LoadCache(ctx)
	if err != nil {
		slog.Warn("failed to load auth token cache", "error", err)
	}
	if tokenSource == nil {
		slog.Warn("no access token available — run `infracost auth login`")
		return nil
	}
	// The LSP outlives a single oauth refresh token: another process
	// (a CLI invocation, a web login) can rotate it while we hold a
	// stale in-memory snapshot. WrapWithReload re-reads the on-disk
	// cache on invalid_grant so the next request recovers
	// transparently instead of failing until the language server is
	// restarted.
	return cfg.WrapWithReload(ctx, tokenSource)
}

func loadPluginsConfig() *cliplugins.Config {
	cfg := &cliplugins.Config{
		AutoUpdate: true,
		BaseURL:    "https://releases.infracost.io",
	}
	loadPluginEnv(cfg)
	cfg.Process()
	return cfg
}

func loadPluginEnv(c *cliplugins.Config) {
	// Per-plugin version pins (INFRACOST_CLI_PLUGIN_<KEY>_VERSION) are read
	// directly by the CLI plugins package, so we only wire the lifecycle knobs
	// here.
	if os.Getenv("INFRACOST_CLI_PLUGIN_MANIFEST_URL") != "" {
		slog.Warn("INFRACOST_CLI_PLUGIN_MANIFEST_URL is deprecated and ignored; set INFRACOST_CLI_PLUGIN_BASE_URL to the release bucket root instead")
	}
	if v := os.Getenv("INFRACOST_CLI_PLUGIN_BASE_URL"); v != "" {
		c.BaseURL = v
	}
	if v := os.Getenv("INFRACOST_CLI_PLUGIN_CACHE_DIRECTORY"); v != "" {
		c.Cache = v
	}
	if v := os.Getenv("INFRACOST_CLI_PLUGIN_DIR"); v != "" {
		c.Dir = v
	}
}
