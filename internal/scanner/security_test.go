package scanner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover FIX-316: an attacker-controlled repo (the editor auto-scans
// a freshly cloned, untrusted workspace) must not be able to use repo-config
// path values (usage_file, project.path) to read, probe, or scan files outside
// the workspace root.

func TestOpenContainedFile_RejectsTraversal(t *testing.T) {
	root := t.TempDir()

	// A secret living outside the workspace root.
	outside := filepath.Join(t.TempDir(), "SECRET_OUTSIDE.txt")
	require.NoError(t, os.WriteFile(outside, []byte("top secret"), 0o600))

	// A legitimate file inside the workspace.
	inside := filepath.Join(root, "usage.yml")
	require.NoError(t, os.WriteFile(inside, []byte("version: 0.1"), 0o600))

	t.Run("out-of-tree read is refused", func(t *testing.T) {
		f, err := openContainedFile(root, "../"+filepath.Base(filepath.Dir(outside))+"/"+filepath.Base(outside))
		if f != nil {
			_ = f.Close()
		}
		assert.Error(t, err, "traversal outside the workspace root must be refused")
	})

	t.Run("absolute path is refused", func(t *testing.T) {
		f, err := openContainedFile(root, outside)
		if f != nil {
			_ = f.Close()
		}
		assert.Error(t, err, "absolute paths must be refused")
	})

	t.Run("existence oracle is closed", func(t *testing.T) {
		// An escaping path must fail identically whether the target exists or
		// not, so a malicious workspace cannot probe for arbitrary files.
		_, errExists := openContainedFile(root, "../"+filepath.Base(filepath.Dir(outside))+"/"+filepath.Base(outside))
		_, errAbsent := openContainedFile(root, "../"+filepath.Base(filepath.Dir(outside))+"/definitely-does-not-exist")
		require.Error(t, errExists)
		require.Error(t, errAbsent)
		assert.Equal(t, os.IsNotExist(errExists), os.IsNotExist(errAbsent),
			"escaping paths must not distinguish existing from absent targets")
	})

	t.Run("symlink escape is refused", func(t *testing.T) {
		link := filepath.Join(root, "link.yml")
		require.NoError(t, os.Symlink(outside, link))
		f, err := openContainedFile(root, "link.yml")
		if f != nil {
			_ = f.Close()
		}
		assert.Error(t, err, "symlink resolving outside the workspace root must be refused")
	})

	t.Run("in-tree file opens", func(t *testing.T) {
		f, err := openContainedFile(root, "usage.yml")
		require.NoError(t, err)
		defer func() { _ = f.Close() }()
		assert.NotNil(t, f)
	})
}

func TestContainedProjectPath(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))

	t.Run("in-tree directory resolves", func(t *testing.T) {
		got, err := containedProjectPath(root, "sub")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(root, "sub"), got)
	})

	t.Run("empty path resolves to root", func(t *testing.T) {
		got, err := containedProjectPath(root, "")
		require.NoError(t, err)
		assert.Equal(t, filepath.Clean(root), got)
	})

	t.Run("traversal out of tree is refused", func(t *testing.T) {
		_, err := containedProjectPath(root, "../../etc")
		assert.Error(t, err, "project path escaping the workspace root must be refused")
	})

	t.Run("absolute path is refused", func(t *testing.T) {
		_, err := containedProjectPath(root, "/etc")
		assert.Error(t, err, "absolute project paths must be refused")
	})
}
