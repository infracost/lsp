package ignore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKey(t *testing.T) {
	k1 := Key("/a/b.tf", "aws_instance.web", "aws-gp3-volumes")
	k2 := Key("/a/b.tf", "aws_instance.web", "aws-gp3-volumes")
	k3 := Key("/a/c.tf", "aws_instance.web", "aws-gp3-volumes")

	assert.Equal(t, k1, k2, "identical inputs should produce identical keys")
	assert.NotEqual(t, k1, k3, "different inputs should produce different keys")
}

func TestStoreAddAndIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ignores.json")
	s, err := NewStoreWithPath(path)
	require.NoError(t, err)

	tests := []struct {
		name     string
		addPath  string
		addRes   string
		addSlug  string
		queryP   string
		queryR   string
		queryS   string
		expected bool
	}{
		{
			name:     "specific match",
			addPath:  "/a/b.tf",
			addRes:   "aws_instance.web",
			addSlug:  "aws-gp3-volumes",
			queryP:   "/a/b.tf",
			queryR:   "aws_instance.web",
			queryS:   "aws-gp3-volumes",
			expected: true,
		},
		{
			name:     "specific no match different file",
			addPath:  "/a/b.tf",
			addRes:   "aws_instance.web",
			addSlug:  "aws-gp3-volumes",
			queryP:   "/a/c.tf",
			queryR:   "aws_instance.web",
			queryS:   "aws-gp3-volumes",
			expected: false,
		},
		{
			name:     "global match",
			addPath:  "*",
			addRes:   "*",
			addSlug:  "aws-gp3-volumes",
			queryP:   "/any/file.tf",
			queryR:   "aws_instance.anything",
			queryS:   "aws-gp3-volumes",
			expected: true,
		},
		{
			name:     "global no match different slug",
			addPath:  "*",
			addRes:   "*",
			addSlug:  "aws-gp3-volumes",
			queryP:   "/any/file.tf",
			queryR:   "aws_instance.anything",
			queryS:   "aws-ebs-snapshot",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Fresh store per test case.
			s.ignores = make(map[string]Entry)
			err := s.Add(tt.addPath, tt.addRes, tt.addSlug)
			require.NoError(t, err)
			got := s.IsIgnored(tt.queryP, tt.queryR, tt.queryS)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestStorePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ignores.json")

	s1, err := NewStoreWithPath(path)
	require.NoError(t, err)
	err = s1.Add("/a/b.tf", "aws_instance.web", "slug1")
	require.NoError(t, err)

	// Load a new store from the same file.
	s2, err := NewStoreWithPath(path)
	require.NoError(t, err)
	assert.True(t, s2.IsIgnored("/a/b.tf", "aws_instance.web", "slug1"), "expected ignore to persist across store instances")
}

func TestStoreRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ignores.json")
	s, err := NewStoreWithPath(path)
	require.NoError(t, err)

	err = s.Add("/a/b.tf", "res", "slug")
	require.NoError(t, err)
	assert.True(t, s.IsIgnored("/a/b.tf", "res", "slug"), "expected ignored after add")

	key := Key("/a/b.tf", "res", "slug")
	err = s.Remove(key)
	require.NoError(t, err)
	assert.False(t, s.IsIgnored("/a/b.tf", "res", "slug"), "expected not ignored after remove")
}

func TestNewStoreMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist", "ignores.json")
	s, err := NewStoreWithPath(path)
	require.NoError(t, err, "missing file should not error")
	assert.False(t, s.IsIgnored("/a", "b", "c"), "empty store should not match anything")
}

func TestNewStoreEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom-ignores.json")
	t.Setenv("INFRACOST_IGNORES_FILE", path)

	s, err := NewStore()
	require.NoError(t, err)
	err = s.Add("/a", "b", "c")
	require.NoError(t, err)

	_, err = os.Stat(path)
	require.NoError(t, err, "expected file at env override path")
}
