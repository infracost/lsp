package scanner

import (
	"os"
	"path/filepath"
	"testing"

	cliplugins "github.com/infracost/cli/pkg/plugins"
	repoconfig "github.com/infracost/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Config detection must look for parser plugins where the scanner loads them from, so a custom
// INFRACOST_CLI_PLUGIN_DIR is honoured rather than config's default plugin dir.
func TestLoadConfigUsesScannerPluginDir(t *testing.T) {
	pluginDir := t.TempDir()
	s := &Scanner{Plugins: &cliplugins.Config{Dir: pluginDir}}
	require.Equal(t, pluginDir, s.PluginDir())

	t.Run("generate", func(t *testing.T) {
		_, err := LoadConfig(t.TempDir(), "", s.PluginDir())
		require.ErrorIs(t, err, repoconfig.ErrNoParserPlugins)
		assert.Contains(t, err.Error(), pluginDir)
	})

	t.Run("scan", func(t *testing.T) {
		_, err := s.Scan(t.Context(), t.TempDir())
		require.ErrorIs(t, err, repoconfig.ErrNoParserPlugins)
		assert.Contains(t, err.Error(), pluginDir)
	})

	t.Run("load typeless config", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "infracost.yml"), []byte(
			"version: 0.1\nprojects:\n  - path: .\n    name: a\n",
		), 0o600))
		_, err := LoadConfig(dir, "", s.PluginDir())
		require.ErrorIs(t, err, repoconfig.ErrNoParserPlugins)
		assert.Contains(t, err.Error(), pluginDir)
	})
}

func TestPluginDirWithoutPluginConfig(t *testing.T) {
	var nilScanner *Scanner
	assert.Empty(t, nilScanner.PluginDir())
	assert.Empty(t, (&Scanner{}).PluginDir())
}
