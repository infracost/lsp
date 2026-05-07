package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	cliplugins "github.com/infracost/cli/pkg/plugins"
	providerconv "github.com/infracost/go-proto/pkg/providers"
	proto "github.com/infracost/proto/gen/go/infracost/provider"
	"golang.org/x/mod/semver"
)

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
	if wantVersion != "" {
		return cfg.Ensure(plugin, wantVersion)
	}

	manifest, err := loadPluginManifest(cfg.ManifestURL)
	if err != nil {
		return cfg.Ensure(plugin, wantVersion)
	}

	entry, ok := manifest.Plugins[plugin]
	if !ok {
		return cfg.Ensure(plugin, wantVersion)
	}

	versions := pluginVersions(entry)
	if len(versions) == 0 {
		return cfg.Ensure(plugin, wantVersion)
	}

	var firstErr error
	for i, version := range versions {
		path, err := cfg.Ensure(plugin, version)
		if err == nil {
			if i > 0 {
				slog.Warn("plugin download fallback succeeded",
					"plugin", plugin,
					"version", version,
					"latest_version", entry.Latest,
				)
			}
			return path, nil
		}

		if firstErr == nil {
			firstErr = err
		}
		if !isDownload404(err) {
			return "", err
		}
	}

	return "", firstErr
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

func pluginVersions(plugin cliplugins.Plugin) []string {
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

	if plugin.Latest != "" {
		return append([]string{plugin.Latest}, versions...)
	}

	return versions
}

func normalizeSemver(version string) string {
	if strings.HasPrefix(version, "v") {
		return version
	}
	return "v" + version
}

func isDownload404(err error) bool {
	return strings.Contains(err.Error(), "unexpected HTTP status: 404 Not Found")
}
