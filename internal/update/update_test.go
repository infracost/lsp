package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSwapBinary(t *testing.T) {
	for _, renameAside := range []bool{false, true} {
		name := "direct rename"
		if renameAside {
			name = "rename aside"
		}

		t.Run(name+" replaces the target and preserves its mode", func(t *testing.T) {
			dir, execPath := installedBinary(t)

			require.NoError(t, swapBinary(execPath, []byte("new"), 0o755, renameAside))

			assert.Equal(t, "new", contents(t, execPath))
			info, err := os.Stat(execPath)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
			assert.Equal(t, []string{"infracost-ls"}, entryNames(t, dir))
		})

		t.Run(name+" keeps the original when the install fails", func(t *testing.T) {
			dir, execPath := installedBinary(t)

			stubRename(t, func(oldpath, newpath string) error {
				if strings.HasPrefix(filepath.Base(oldpath), updateTmpPrefix) {
					return os.ErrPermission
				}
				return os.Rename(oldpath, newpath)
			})

			err := swapBinary(execPath, []byte("new"), 0o755, renameAside)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "installing new binary")

			assert.Equal(t, "old", contents(t, execPath))
			assert.Equal(t, []string{"infracost-ls"}, entryNames(t, dir))
		})
	}

	t.Run("errors and keeps the original when it cannot be moved aside", func(t *testing.T) {
		dir, execPath := installedBinary(t)

		stubRename(t, func(oldpath, newpath string) error {
			if filepath.Base(oldpath) == "infracost-ls" {
				return os.ErrPermission
			}
			return os.Rename(oldpath, newpath)
		})

		err := swapBinary(execPath, []byte("new"), 0o755, true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "moving current binary aside")

		assert.Equal(t, "old", contents(t, execPath))
		assert.Equal(t, []string{"infracost-ls"}, entryNames(t, dir))
	})

	t.Run("preserves the displaced binary when rollback fails", func(t *testing.T) {
		dir, execPath := installedBinary(t)

		stubRename(t, func(oldpath, newpath string) error {
			if filepath.Base(newpath) == "infracost-ls" {
				return os.ErrPermission
			}
			return os.Rename(oldpath, newpath)
		})

		err := swapBinary(execPath, []byte("new"), 0o755, true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "rollback failed")

		bak := execPath + ".bak"
		assert.Contains(t, err.Error(), bak)
		assert.NoFileExists(t, execPath)
		assert.Equal(t, "old", contents(t, bak))
		assert.Equal(t, []string{"infracost-ls.bak"}, entryNames(t, dir))

		// the reported path must outlive the next startup sweep
		age(t, bak, time.Hour)
		require.NoError(t, cleanupStale(dir))
		assert.Equal(t, "old", contents(t, bak))
	})
}

func TestCleanupStale(t *testing.T) {
	dir := t.TempDir()

	keep := []string{"infracost-ls", "infracost-ls.exe", "infracost-ls.bak", "config.json"}
	remove := []string{updateOldPrefix + "123", updateTmpPrefix + "456"}
	recent := []string{updateOldPrefix + "inflight", updateTmpPrefix + "inflight"}

	for _, name := range append(append(append([]string{}, keep...), remove...), recent...) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644))
	}
	require.NoError(t, os.Mkdir(filepath.Join(dir, updateOldPrefix+"dir"), 0o755))

	for _, name := range append(append([]string{}, keep...), remove...) {
		age(t, filepath.Join(dir, name), time.Hour)
	}

	require.NoError(t, cleanupStale(dir))

	want := append(append([]string{}, keep...), recent...)
	assert.ElementsMatch(t, append(want, updateOldPrefix+"dir"), entryNames(t, dir))
}

func installedBinary(t *testing.T) (dir, execPath string) {
	t.Helper()
	dir = t.TempDir()
	execPath = filepath.Join(dir, "infracost-ls")
	require.NoError(t, os.WriteFile(execPath, []byte("old"), 0o755))
	return dir, execPath
}

func stubRename(t *testing.T, fn func(oldpath, newpath string) error) {
	t.Helper()
	original := renameFile
	renameFile = fn
	t.Cleanup(func() { renameFile = original })
}

func age(t *testing.T, path string, by time.Duration) {
	t.Helper()
	old := time.Now().Add(-by)
	require.NoError(t, os.Chtimes(path, old, old))
}

func contents(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
