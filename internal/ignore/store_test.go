package ignore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKey(t *testing.T) {
	k1 := Key("/a/b.tf", "aws_instance.web", "aws-gp3-volumes")
	k2 := Key("/a/b.tf", "aws_instance.web", "aws-gp3-volumes")
	k3 := Key("/a/c.tf", "aws_instance.web", "aws-gp3-volumes")

	if k1 != k2 {
		t.Fatal("identical inputs should produce identical keys")
	}
	if k1 == k3 {
		t.Fatal("different inputs should produce different keys")
	}
}

func TestStoreAddAndIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ignores.json")
	s, err := NewStoreWithPath(path)
	if err != nil {
		t.Fatal(err)
	}

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
			if err := s.Add(tt.addPath, tt.addRes, tt.addSlug); err != nil {
				t.Fatal(err)
			}
			got := s.IsIgnored(tt.queryP, tt.queryR, tt.queryS)
			if got != tt.expected {
				t.Errorf("IsIgnored(%q, %q, %q) = %v, want %v", tt.queryP, tt.queryR, tt.queryS, got, tt.expected)
			}
		})
	}
}

func TestStorePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ignores.json")

	s1, err := NewStoreWithPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Add("/a/b.tf", "aws_instance.web", "slug1"); err != nil {
		t.Fatal(err)
	}

	// Load a new store from the same file.
	s2, err := NewStoreWithPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.IsIgnored("/a/b.tf", "aws_instance.web", "slug1") {
		t.Fatal("expected ignore to persist across store instances")
	}
}

func TestStoreRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ignores.json")
	s, err := NewStoreWithPath(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Add("/a/b.tf", "res", "slug"); err != nil {
		t.Fatal(err)
	}
	if !s.IsIgnored("/a/b.tf", "res", "slug") {
		t.Fatal("expected ignored after add")
	}

	key := Key("/a/b.tf", "res", "slug")
	if err := s.Remove(key); err != nil {
		t.Fatal(err)
	}
	if s.IsIgnored("/a/b.tf", "res", "slug") {
		t.Fatal("expected not ignored after remove")
	}
}

func TestNewStoreMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist", "ignores.json")
	s, err := NewStoreWithPath(path)
	if err != nil {
		t.Fatal("missing file should not error", err)
	}
	if s.IsIgnored("/a", "b", "c") {
		t.Fatal("empty store should not match anything")
	}
}

func TestNewStoreEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom-ignores.json")
	t.Setenv("INFRACOST_IGNORES_FILE", path)

	s, err := NewStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add("/a", "b", "c"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file at env override path: %v", err)
	}
}
