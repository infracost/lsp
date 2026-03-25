package config

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hashicorp/go-hclog"
	"github.com/infracost/cli/pkg/auth"
	"github.com/infracost/cli/pkg/environment"
	cliplugins "github.com/infracost/cli/pkg/plugins"
	"golang.org/x/oauth2"
)

type Config struct {
	LogLevel        hclog.Level
	SlogLevel       slog.Level
	Currency        string
	PricingEndpoint string
	DebugUI         string

	TokenSource       oauth2.TokenSource
	DashboardEndpoint string
	Plugins           cliplugins.Config
}

func Load(ctx context.Context) Config {
	cfg := Config{
		LogLevel:        hclog.Warn,
		SlogLevel:       slog.LevelInfo,
		Currency:        "USD",
		PricingEndpoint: "https://pricing.api.infracost.io",
	}

	if os.Getenv("INFRACOST_LOG_LEVEL") == "debug" {
		cfg.LogLevel = hclog.Debug
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
		slog.Warn("no access token available — run `infracost login`")
	}
	return tokenSource
}

func loadPluginsConfig() cliplugins.Config {
	cfg := cliplugins.Config{
		AutoUpdate:  true,
		ManifestURL: "https://releases.infracost.io/plugins/manifest.json",
	}
	loadPluginEnv(&cfg)
	cfg.Process()
	return cfg
}

func loadPluginEnv(c *cliplugins.Config) {
	if v := os.Getenv("INFRACOST_CLI_PARSER_PLUGIN"); v != "" {
		c.Parser.Plugin = v
	}
	if v := os.Getenv("INFRACOST_CLI_PARSER_PLUGIN_VERSION"); v != "" {
		c.Parser.Version = v
	}
	if v := os.Getenv("INFRACOST_CLI_PROVIDER_PLUGIN_AWS"); v != "" {
		c.Providers.AWS = v
	}
	if v := os.Getenv("INFRACOST_CLI_PROVIDER_PLUGIN_GOOGLE"); v != "" {
		c.Providers.Google = v
	}
	if v := os.Getenv("INFRACOST_CLI_PROVIDER_PLUGIN_AZURERM"); v != "" {
		c.Providers.Azure = v
	}
	if v := os.Getenv("INFRACOST_CLI_PROVIDER_PLUGIN_AWS_VERSION"); v != "" {
		c.Providers.AWSVersion = v
	}
	if v := os.Getenv("INFRACOST_CLI_PROVIDER_PLUGIN_GOOGLE_VERSION"); v != "" {
		c.Providers.GoogleVersion = v
	}
	if v := os.Getenv("INFRACOST_CLI_PROVIDER_PLUGIN_AZURE_VERSION"); v != "" {
		c.Providers.AzureVersion = v
	}
	if v := os.Getenv("INFRACOST_CLI_PLUGIN_MANIFEST_URL"); v != "" {
		c.ManifestURL = v
	}
	if v := os.Getenv("INFRACOST_CLI_PLUGIN_CACHE_DIRECTORY"); v != "" {
		c.Cache = v
	}
}
