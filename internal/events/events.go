package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/google/uuid"

	"github.com/infracost/lsp/internal/trace"
	"github.com/infracost/lsp/version"
)

type Client interface {
	Push(ctx context.Context, event string, extra ...interface{})
	SetTokenSource(ts TokenSource)
}

func NewClient(httpClient *http.Client, endpoint string) Client {
	return &client{
		httpClient: httpClient,
		endpoint:   endpoint,
		userAgent:  trace.UserAgent,
	}
}

type client struct {
	httpClient  *http.Client
	endpoint    string
	userAgent   string
	tokenSource TokenSource
}

// TokenSource provides an access token for authenticating event requests.
type TokenSource interface {
	Token() (string, error)
}

// SetTokenSource sets the token source used to authenticate event requests.
func (c *client) SetTokenSource(ts TokenSource) {
	c.tokenSource = ts
}

func (c *client) Push(ctx context.Context, event string, extra ...interface{}) {
	if len(extra)%2 != 0 {
		panic("events.Push: extra args must be key-value pairs")
	}

	metadataMu.RLock()
	env := make(map[string]interface{}, len(metadata)+len(extra)/2)
	for k, v := range metadata {
		env[k] = v
	}
	metadataMu.RUnlock()
	for i := 0; i < len(extra); i += 2 {
		key, ok := extra[i].(string)
		if !ok {
			panic(fmt.Sprintf("events.Push: extra arg %d must be a string key", i))
		}
		env[key] = extra[i+1]
	}

	body := struct {
		Event string                 `json:"event"`
		Env   map[string]interface{} `json:"env"`
	}{
		Event: event,
		Env:   env,
	}

	buf, err := json.Marshal(body)
	if err != nil {
		slog.Warn("events: failed to marshal event", "error", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/event", c.endpoint), bytes.NewReader(buf))
	if err != nil {
		slog.Warn("events: failed to create request", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if c.tokenSource != nil {
		if token, err := c.tokenSource.Token(); err == nil {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}

	slog.Debug("events: sending event", "event", event)

	resp, err := c.httpClient.Do(req) //nolint:gosec // endpoint is from config, not user input
	if err != nil {
		slog.Warn("events: failed to send event", "error", err)
		return
	}
	_ = resp.Body.Close()

	slog.Debug("events: event sent", "event", event, "status", resp.StatusCode)
}

var (
	metadata   map[string]interface{}
	metadataMu sync.RWMutex
)

func init() {
	id := loadInstallID()
	metadata = map[string]interface{}{
		"caller":      "infracost-ls",
		"version":     version.Version,
		"fullVersion": version.Version,
		"id":          id,
		"installId":   id,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
	}
}

// RegisterMetadata adds or updates entries in the global event metadata.
func RegisterMetadata(key string, value interface{}) {
	metadataMu.Lock()
	metadata[key] = value
	metadataMu.Unlock()
}

// GetMetadata retrieves a typed metadata value. Returns false if the key
// doesn't exist or the type doesn't match.
func GetMetadata[V any](key string) (V, bool) {
	metadataMu.RLock()
	value, ok := metadata[key]
	metadataMu.RUnlock()
	if !ok {
		var v V
		return v, false
	}
	v, ok := value.(V)
	return v, ok
}

// loadInstallID reads or creates a persistent install ID, sharing the same
// file as the CLI (~/.config/infracost/installId).
func loadInstallID() string {
	path := installIDPath()

	data, err := os.ReadFile(filepath.Clean(path))
	if err == nil {
		return string(data)
	}

	if !os.IsNotExist(err) {
		slog.Warn("events: failed to read install ID", "error", err)
		return uuid.Nil.String()
	}

	id := uuid.New().String()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		slog.Warn("events: failed to create install ID directory", "error", err)
		return id
	}
	if err := os.WriteFile(path, []byte(id), 0600); err != nil {
		slog.Warn("events: failed to save install ID", "error", err)
	}
	return id
}

func installIDPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "infracost", "installId")
	}
	if dir, err := os.UserHomeDir(); err == nil {
		return filepath.Join(dir, ".infracost", "installId")
	}
	return filepath.Join(".infracost", "installId")
}
