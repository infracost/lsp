// Package pluginlog forwards hashicorp/go-hclog plugin logs into the LSP's
// slog stream so go-plugin handshake diagnostics flow through the same
// pipeline as the rest of the server (and the v0.2.0 debug trace recorder).
package pluginlog

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/hashicorp/go-hclog"
	"github.com/infracost/cli/pkg/plugins/pluginerr"
)

// New returns an hclog.Logger that forwards every emitted line to logger as a
// structured slog entry.
//
// hclog level is pinned to Trace so go-plugin's handshake events (emitted at
// Debug/Trace) reach the slog stream — and from there the v0.2.0 debug trace
// recorder. The recorder's log store is a bounded ring buffer (5000 entries),
// so unconditional Trace is safe. Verbosity at the slog destination is still
// controlled by the slog handler that ultimately receives the records.
func New(logger *slog.Logger) hclog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	return hclog.New(&hclog.LoggerOptions{
		Level:      hclog.Trace,
		Output:     &slogWriter{log: logger},
		JSONFormat: true,
	})
}

// LogConnectError emits a category-tagged error log for a plugin connect
// failure so the failure mode (AV exec block, firewall handshake timeout, etc.)
// is obvious without parsing the raw error string.
func LogConnectError(kind, path string, err error) {
	if err == nil {
		return
	}
	switch {
	case errors.Is(err, pluginerr.ErrPluginExecFailed):
		slog.Error("plugin process failed to start (likely AV/EDR blocking the binary)", "kind", kind, "path", path, "error", err)
	case errors.Is(err, pluginerr.ErrPluginHandshakeTimeout):
		slog.Error("plugin handshake timed out (likely firewall/EDR blocking loopback TCP)", "kind", kind, "path", path, "error", err)
	case errors.Is(err, pluginerr.ErrPluginNotFound):
		slog.Error("plugin binary not found", "kind", kind, "path", path, "error", err)
	case errors.Is(err, pluginerr.ErrPluginNotExecutable):
		slog.Error("plugin binary not executable", "kind", kind, "path", path, "error", err)
	case errors.Is(err, pluginerr.ErrPluginHandshake):
		slog.Error("plugin handshake failed", "kind", kind, "path", path, "error", err)
	default:
		slog.Error("plugin connect failed", "kind", kind, "path", path, "error", err)
	}
}

type slogWriter struct {
	log *slog.Logger
}

func (w *slogWriter) Write(p []byte) (int, error) {
	raw := map[string]any{}
	if err := json.Unmarshal(p, &raw); err != nil {
		w.log.Debug(strings.TrimSpace(string(p)), "source", "go-plugin")
		return len(p), nil
	}

	msg, _ := raw["@message"].(string)
	levelStr, _ := raw["@level"].(string)

	attrs := []slog.Attr{slog.String("source", "go-plugin")}
	for k, v := range raw {
		switch k {
		case "@message", "@level", "@timestamp", "@caller":
			continue
		case "@module":
			if s, ok := v.(string); ok {
				attrs = append(attrs, slog.String("module", s))
			}
		default:
			attrs = append(attrs, slog.Any(k, v))
		}
	}

	w.log.LogAttrs(context.Background(), mapLevel(levelStr), msg, attrs...)
	return len(p), nil
}

func mapLevel(hclogLevel string) slog.Level {
	switch hclogLevel {
	case "trace", "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelDebug
	}
}
