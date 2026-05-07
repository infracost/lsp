package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"

	cliplugins "github.com/infracost/cli/pkg/plugins"
	providerconv "github.com/infracost/go-proto/pkg/providers"
	proto "github.com/infracost/proto/gen/go/infracost/provider"
	"golang.org/x/mod/semver"
)

var download404Pattern = regexp.MustCompile(`unexpected HTTP status:\s*404\b`)

// EnsureParserPlugin resolves the parser plugin, retrying older manifest
// versions when the unpinned latest artifact is published with a broken URL.
func EnsureParserPlugin(cfg *cliplugins.Config) error {
	if cfg.Parser.Plugin != "" {
		return nil
	}

	path, err := ensurePluginWithFallback(cfg, "infracost-parser-plugin", cfg.Parser.Version)
	if err != nil {
		return err
	}

	cfg.Parser.Plugin = path
	return nil
}

// EnsureProviderPlugin resolves the requested provider plugin, retrying older
// manifest versions when the unpinned latest artifact is published with a
// broken URL.
func EnsureProviderPlugin(cfg *cliplugins.Config, provider proto.Provider) error {
	var (
		override string
		version  string
		plugin   string
	)

	switch provider {
	case proto.Provider_PROVIDER_AWS:
		override = cfg.Providers.AWS
		version = cfg.Providers.AWSVersion
	case proto.Provider_PROVIDER_GOOGLE:
		override = cfg.Providers.Google
		version = cfg.Providers.GoogleVersion
	case proto.Provider_PROVIDER_AZURERM:
		override = cfg.Providers.Azure
		version = cfg.Providers.AzureVersion
	default:
		return fmt.Errorf("unknown provider: %s", providerconv.FromProto(provider))
	}

	if override != "" {
		return nil
	}

	plugin = fmt.Sprintf("infracost-provider-plugin-%s", providerconv.FromProto(provider))
	path, err := ensurePluginWithFallback(cfg, plugin, version)
	if err != nil {
		return err
	}

	switch provider {
	case proto.Provider_PROVIDER_AWS:
		cfg.Providers.AWS = path
	case proto.Provider_PROVIDER_GOOGLE:
		cfg.Providers.Google = path
	case proto.Provider_PROVIDER_AZURERM:
		cfg.Providers.Azure = path
	}

	return nil
}

func ensurePluginWithFallback(cfg *cliplugins.Config, plugin, wantVersion string) (string, error) {
	path, err := cfg.Ensure(plugin, wantVersion)
	if err == nil {
		return path, nil
	}
	initialErr := err

	if wantVersion != "" || !cfg.AutoUpdate || !isDownload404(initialErr) {
		return "", initialErr
	}

	manifest, err := loadPluginManifest(cfg.ManifestURL)
	if err != nil {
		return "", fmt.Errorf("reloading plugin manifest for fallback after %q download failed: %w", plugin, err)
	}

	entry, ok := manifest.Plugins[plugin]
	if !ok {
		return "", fmt.Errorf("plugin %q not found in manifest at %s while attempting fallback", plugin, cfg.ManifestURL)
	}

	versions := fallbackVersions(entry)
	if len(versions) == 0 {
		return "", fmt.Errorf("plugin %q has no fallback versions in manifest at %s", plugin, cfg.ManifestURL)
	}

	lastErr := initialErr
	for _, version := range versions {
		path, err := cfg.Ensure(plugin, version)
		if err == nil {
			slog.Warn("plugin download fallback succeeded",
				"plugin", plugin,
				"version", version,
				"latest_version", entry.Latest,
			)
			return path, nil
		}
		lastErr = err
		if !isDownload404(err) {
			return "", err
		}
	}

	return "", fmt.Errorf("no working fallback download found for plugin %q: %w", plugin, lastErr)
}

func loadPluginManifest(manifestURL string) (*cliplugins.Manifest, error) {
	resp, err := http.Get(manifestURL) //nolint:gosec // URL comes from trusted config/env.
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch plugin manifest (%s): %s", manifestURL, resp.Status)
	}

	var manifest cliplugins.Manifest
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return nil, err
	}

	return &manifest, nil
}

func fallbackVersions(plugin cliplugins.Plugin) []string {
	versions := make([]string, 0, len(plugin.Versions))
	for version := range plugin.Versions {
		if version == plugin.Latest {
			continue
		}
		versions = append(versions, version)
	}

	sort.Slice(versions, func(i, j int) bool {
		vi := normalizeSemver(versions[i])
		vj := normalizeSemver(versions[j])
		if cmp := semver.Compare(vi, vj); cmp != 0 {
			return cmp > 0
		}
		return versions[i] > versions[j]
	})

	return versions
}

func normalizeSemver(version string) string {
	if strings.HasPrefix(version, "v") {
		return version
	}
	return "v" + version
}

func isDownload404(err error) bool {
	if err == nil {
		return false
	}

	return download404Pattern.MatchString(err.Error())
}
