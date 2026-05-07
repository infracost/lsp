package config

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	cliplugins "github.com/infracost/cli/pkg/plugins"
	proto "github.com/infracost/proto/gen/go/infracost/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureParserPluginUsesCachedVersionWhenAutoUpdateDisabled(t *testing.T) {
	t.Parallel()

	plugin := "infracost-parser-plugin"
	platform := runtime.GOOS + "_" + runtime.GOARCH
	cacheDir := t.TempDir()

	for _, version := range []string{"0.1.0", "0.3.0", "0.2.0"} {
		dir := filepath.Join(cacheDir, plugin, platform, version)
		require.NoError(t, os.MkdirAll(dir, 0750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, testBinaryName(plugin)), []byte(version), 0750))
	}

	var manifestHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		manifestHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := &cliplugins.Config{
		Cache:       cacheDir,
		ManifestURL: srv.URL + "/manifest.json",
		AutoUpdate:  false,
	}

	err := EnsureParserPlugin(cfg)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(cacheDir, plugin, platform, "0.3.0", testBinaryName(plugin)), cfg.Parser.Plugin)
	assert.Zero(t, manifestHits.Load())
}

func TestEnsureParserPluginFallsBackAfterLatest404(t *testing.T) {
	t.Parallel()

	plugin := "infracost-parser-plugin"
	cfg := &cliplugins.Config{
		Cache:      t.TempDir(),
		AutoUpdate: true,
	}

	srv := newPluginServer(t, plugin, "0.3.0", "0.2.0")
	defer srv.Close()
	cfg.ManifestURL = srv.URL + "/manifest.json"

	err := EnsureParserPlugin(cfg)
	require.NoError(t, err)
	assert.Contains(t, cfg.Parser.Plugin, filepath.Join(plugin, runtime.GOOS+"_"+runtime.GOARCH, "0.2.0"))
}

func TestEnsureProviderPluginFallsBackAfterLatest404(t *testing.T) {
	t.Parallel()

	plugin := "infracost-provider-plugin-aws"
	cfg := &cliplugins.Config{
		Cache:      t.TempDir(),
		AutoUpdate: true,
	}

	srv := newPluginServer(t, plugin, "0.3.0", "0.2.0")
	defer srv.Close()
	cfg.ManifestURL = srv.URL + "/manifest.json"

	err := EnsureProviderPlugin(cfg, proto.Provider_PROVIDER_AWS)
	require.NoError(t, err)
	assert.Contains(t, cfg.Providers.AWS, filepath.Join(plugin, runtime.GOOS+"_"+runtime.GOARCH, "0.2.0"))
}

func TestEnsureParserPluginErrorsWhenPluginMissingFromManifest(t *testing.T) {
	t.Parallel()

	cfg := &cliplugins.Config{
		Cache:      t.TempDir(),
		AutoUpdate: true,
	}

	manifest := cliplugins.Manifest{
		Plugins: map[string]cliplugins.Plugin{
			"other-plugin": {
				Latest: "0.2.0",
			},
		},
	}
	srv := newManifestServer(t, manifest, map[string]serverResponse{
		"/broken": {status: http.StatusNotFound},
	})
	defer srv.Close()
	cfg.ManifestURL = srv.URL + "/manifest.json"

	err := EnsureParserPlugin(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `plugin "infracost-parser-plugin" not found in manifest`)
	assert.Empty(t, cfg.Parser.Plugin)
}

func TestEnsureParserPluginErrorsWhenNoFallbackVersionsExist(t *testing.T) {
	t.Parallel()

	plugin := "infracost-parser-plugin"
	cfg := &cliplugins.Config{
		Cache:      t.TempDir(),
		AutoUpdate: true,
	}

	manifest := manifestForPlugin(plugin, "0.3.0", cliplugins.Version{
		Artifacts: map[string]cliplugins.Artifact{
			testPlatform(): {
				URL:  "http://example.invalid/broken",
				SHA:  "ignored",
				Name: pluginArchiveName(plugin),
			},
		},
	})
	srv := newManifestServer(t, manifest, map[string]serverResponse{
		"/broken": {status: http.StatusNotFound},
	})
	defer srv.Close()
	replaceArtifactURL(manifest.Plugins[plugin].Versions["0.3.0"].Artifacts, testPlatform(), srv.URL+"/broken")

	cfg.ManifestURL = srv.URL + "/manifest.json"

	err := EnsureParserPlugin(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `plugin "infracost-parser-plugin" has no fallback versions`)
	assert.Empty(t, cfg.Parser.Plugin)
}

func TestEnsureParserPluginErrorsWhenAllFallbackDownloads404(t *testing.T) {
	t.Parallel()

	plugin := "infracost-parser-plugin"
	cfg := &cliplugins.Config{
		Cache:      t.TempDir(),
		AutoUpdate: true,
	}

	manifest := manifestForPlugin(plugin, "0.3.0",
		cliplugins.Version{
			Artifacts: map[string]cliplugins.Artifact{
				testPlatform(): {
					URL:  "http://example.invalid/broken-latest",
					SHA:  "ignored",
					Name: pluginArchiveName(plugin),
				},
			},
		},
		cliplugins.Version{
			Artifacts: map[string]cliplugins.Artifact{
				testPlatform(): {
					URL:  "http://example.invalid/broken-fallback",
					SHA:  "ignored",
					Name: pluginArchiveName(plugin),
				},
			},
		},
	)
	srv := newManifestServer(t, manifest, map[string]serverResponse{
		"/broken-latest":   {status: http.StatusNotFound},
		"/broken-fallback": {status: http.StatusNotFound},
	})
	defer srv.Close()
	replaceArtifactURL(manifest.Plugins[plugin].Versions["0.3.0"].Artifacts, testPlatform(), srv.URL+"/broken-latest")
	replaceArtifactURL(manifest.Plugins[plugin].Versions["0.2.0"].Artifacts, testPlatform(), srv.URL+"/broken-fallback")

	cfg.ManifestURL = srv.URL + "/manifest.json"

	err := EnsureParserPlugin(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no working fallback download found for plugin "infracost-parser-plugin"`)
	assert.Empty(t, cfg.Parser.Plugin)
}

func newPluginServer(t *testing.T, plugin, latestVersion, fallbackVersion string) *httptest.Server {
	t.Helper()

	platform := testPlatform()
	archiveName, archiveData := createPluginArchive(t, plugin, []byte("plugin-binary"))

	manifest := manifestForPlugin(plugin, latestVersion,
		cliplugins.Version{
			Artifacts: map[string]cliplugins.Artifact{
				platform: {
					URL:  "http://example.invalid/broken",
					SHA:  "ignored",
					Name: archiveName,
				},
			},
		},
		cliplugins.Version{
			Artifacts: map[string]cliplugins.Artifact{
				platform: {
					URL:  "http://example.invalid/download",
					SHA:  sha256Hex(archiveData),
					Name: archiveName,
				},
			},
		},
	)
	srv := newManifestServer(t, manifest, map[string]serverResponse{
		"/broken":   {status: http.StatusNotFound},
		"/download": {status: http.StatusOK, body: archiveData},
	})
	replaceArtifactURL(manifest.Plugins[plugin].Versions[latestVersion].Artifacts, platform, srv.URL+"/broken")
	replaceArtifactURL(manifest.Plugins[plugin].Versions[fallbackVersion].Artifacts, platform, srv.URL+"/download")
	return srv
}

type serverResponse struct {
	status int
	body   []byte
}

func newManifestServer(t *testing.T, manifest cliplugins.Manifest, routes map[string]serverResponse) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest.json" {
			w.Header().Set("Content-Type", "application/json")
			require.NoError(t, json.NewEncoder(w).Encode(manifest))
			return
		}

		resp, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}

		w.WriteHeader(resp.status)
		if len(resp.body) > 0 {
			_, _ = w.Write(resp.body)
		}
	}))
}

func manifestForPlugin(plugin, latestVersion string, versions ...cliplugins.Version) cliplugins.Manifest {
	versionMap := make(map[string]cliplugins.Version, len(versions))
	current := latestVersion
	for _, version := range versions {
		versionMap[current] = version
		current = decrementMinorVersion(current)
	}

	return cliplugins.Manifest{
		Plugins: map[string]cliplugins.Plugin{
			plugin: {
				Latest:   latestVersion,
				Versions: versionMap,
			},
		},
	}
}

func decrementMinorVersion(version string) string {
	switch version {
	case "0.3.0":
		return "0.2.0"
	case "0.2.0":
		return "0.1.0"
	default:
		return "0.0.0"
	}
}

func replaceArtifactURL(artifacts map[string]cliplugins.Artifact, platform, url string) {
	artifact := artifacts[platform]
	artifact.URL = url
	artifacts[platform] = artifact
}

func testPlatform() string {
	return runtime.GOOS + "_" + runtime.GOARCH
}

func pluginArchiveName(plugin string) string {
	if runtime.GOOS == "windows" {
		return plugin + "-windows-test.zip"
	}

	return plugin + "-test.tar.gz"
}

func createPluginArchive(t *testing.T, plugin string, content []byte) (string, []byte) {
	t.Helper()

	if runtime.GOOS == "windows" {
		return plugin + "-windows-test.zip", createZipArchive(t, testBinaryName(plugin), content)
	}

	return plugin + "-test.tar.gz", createTarGzArchive(t, plugin, content)
}

func createTarGzArchive(t *testing.T, name string, content []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0750,
		Size: int64(len(content)),
	}))
	_, err := tw.Write(content)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())

	return buf.Bytes()
}

func createZipArchive(t *testing.T, name string, content []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	f, err := zw.Create(name)
	require.NoError(t, err)
	_, err = f.Write(content)
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	return buf.Bytes()
}

func testBinaryName(plugin string) string {
	if runtime.GOOS == "windows" {
		return plugin + ".exe"
	}

	return plugin
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
