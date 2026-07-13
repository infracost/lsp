package lsp

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingHandler captures the messages it handles and only enables records
// at or above minLevel, so tests can assert per-handler level routing.
type recordingHandler struct {
	minLevel slog.Level
	msgs     *[]string
}

func newRecordingHandler(minLevel slog.Level) recordingHandler {
	msgs := []string{}
	return recordingHandler{minLevel: minLevel, msgs: &msgs}
}

func (h recordingHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.minLevel
}

func (h recordingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.msgs = append(*h.msgs, r.Message)
	return nil
}

func (h recordingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(_ string) slog.Handler      { return h }

func TestFanoutHandlerRoutesByPerHandlerLevel(t *testing.T) {
	stderr := newRecordingHandler(slog.LevelInfo)
	capture := newRecordingHandler(slog.LevelDebug)

	logger := slog.New(newFanoutHandler(stderr, capture))

	logger.Debug("debug-line")
	logger.Info("info-line")

	// The info-level handler only sees info; the debug-level handler sees both.
	assert.Equal(t, []string{"info-line"}, *stderr.msgs)
	assert.Equal(t, []string{"debug-line", "info-line"}, *capture.msgs)
}

func TestFanoutHandlerEnabledIsUnionOfSubHandlers(t *testing.T) {
	h := newFanoutHandler(
		newRecordingHandler(slog.LevelInfo),
		newRecordingHandler(slog.LevelDebug),
	)

	// Debug is enabled because at least one sub-handler wants it.
	assert.True(t, h.Enabled(context.Background(), slog.LevelDebug))
	assert.True(t, h.Enabled(context.Background(), slog.LevelError))
}

func TestFanoutHandlerWithAttrsPropagates(t *testing.T) {
	capture := newRecordingHandler(slog.LevelDebug)
	logger := slog.New(newFanoutHandler(capture)).With("key", "value")

	logger.Info("attributed")

	require.Len(t, *capture.msgs, 1)
	assert.Equal(t, "attributed", (*capture.msgs)[0])
}
