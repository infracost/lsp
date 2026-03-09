package ignore

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Entry struct {
	Path      string    `json:"path"`
	Resource  string    `json:"resource"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"created_at"`
}

type storeFile struct {
	Version int              `json:"version"`
	Ignores map[string]Entry `json:"ignores"`
}

type Store struct {
	mu      sync.RWMutex
	path    string
	ignores map[string]Entry
}

func NewStore() (*Store, error) {
	path := os.Getenv("INFRACOST_IGNORES_FILE")
	if path == "" {
		path = defaultPath()
	}
	return NewStoreWithPath(path)
}

func NewStoreWithPath(path string) (*Store, error) {
	s := &Store{
		path:    path,
		ignores: make(map[string]Entry),
	}
	if err := s.load(); err != nil {
		return s, fmt.Errorf("loading ignores: %w", err)
	}
	return s, nil
}

// IsIgnored returns true if the violation should be suppressed.
// Checks both a specific key (path+resource+slug) and a global key (*+*+slug).
func (s *Store) IsIgnored(absPath, resource, slug string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.ignores[Key("*", "*", slug)]; ok {
		return true
	}
	if _, ok := s.ignores[Key(absPath, resource, slug)]; ok {
		return true
	}
	return false
}

// Add persists an ignore entry to disk.
func (s *Store) Add(absPath, resource, slug string) error {
	key := Key(absPath, resource, slug)
	entry := Entry{
		Path:      absPath,
		Resource:  resource,
		Slug:      slug,
		CreatedAt: time.Now().UTC(),
	}

	s.mu.Lock()
	s.ignores[key] = entry
	s.mu.Unlock()

	return s.save()
}

// Remove deletes an ignore entry by its hash key.
func (s *Store) Remove(key string) error {
	s.mu.Lock()
	delete(s.ignores, key)
	s.mu.Unlock()

	return s.save()
}

// Key computes the SHA-256 hash for an ignore entry.
func Key(absPath, resource, slug string) string {
	h := sha256.Sum256([]byte(absPath + "\x00" + resource + "\x00" + slug))
	return fmt.Sprintf("%x", h)
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	if f.Ignores != nil {
		s.ignores = f.Ignores
	}
	return nil
}

func (s *Store) save() error {
	s.mu.RLock()
	snapshot := make(map[string]Entry, len(s.ignores))
	for k, v := range s.ignores {
		snapshot[k] = v
	}
	s.mu.RUnlock()

	f := storeFile{
		Version: 1,
		Ignores: snapshot,
	}

	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0750); err != nil {
		return err
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func defaultPath() string {
	dir, err := os.UserConfigDir()
	if err == nil {
		return filepath.Join(dir, "infracost", "ignores.json")
	}
	slog.Warn("failed to get user config dir, falling back to home directory", "error", err)

	dir, err = os.UserHomeDir()
	if err == nil {
		return filepath.Join(dir, ".infracost", "ignores.json")
	}
	slog.Warn("failed to get user home dir, falling back to current directory", "error", err)

	return filepath.Join(".infracost", "ignores.json")
}
