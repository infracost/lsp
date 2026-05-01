package pluginlog

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewForwardsToSlog(t *testing.T) {
	var buf bytes.Buffer
	slogLogger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	hclogLogger := New(slogLogger)
	hclogLogger.Debug("starting plugin", "path", "/tmp/p", "pid", 1234)

	var entry map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry))

	assert.Equal(t, "starting plugin", entry["msg"])
	assert.Equal(t, "DEBUG", entry["level"])
	assert.Equal(t, "go-plugin", entry["source"])
	assert.Equal(t, "/tmp/p", entry["path"])
	assert.EqualValues(t, 1234, entry["pid"])
}

func TestNewMapsLevels(t *testing.T) {
	tests := []struct {
		hclogLevel string
		want       string
	}{
		{"trace", "DEBUG"},
		{"debug", "DEBUG"},
		{"info", "INFO"},
		{"warn", "WARN"},
		{"error", "ERROR"},
	}

	for _, tc := range tests {
		t.Run(tc.hclogLevel, func(t *testing.T) {
			var buf bytes.Buffer
			slogLogger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			hclogLogger := New(slogLogger)

			switch tc.hclogLevel {
			case "trace":
				hclogLogger.Trace("m")
			case "debug":
				hclogLogger.Debug("m")
			case "info":
				hclogLogger.Info("m")
			case "warn":
				hclogLogger.Warn("m")
			case "error":
				hclogLogger.Error("m")
			}

			var entry map[string]any
			require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry))
			assert.Equal(t, tc.want, entry["level"])
		})
	}
}
