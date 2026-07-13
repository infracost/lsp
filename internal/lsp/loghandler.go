package lsp

import (
	"context"
	"errors"
	"log/slog"
)

// fanoutHandler dispatches each log record to several slog.Handlers. It lets the
// server keep writing to stderr (at the level configured from
// INFRACOST_LOG_LEVEL) while also feeding the debug-capture handler used by
// infracost/exportTrace, instead of one replacing the other.
//
// Each sub-handler is consulted for its own Enabled level, so a verbose capture
// handler doesn't force debug lines onto a stderr handler configured for info.
type fanoutHandler struct {
	handlers []slog.Handler
}

func newFanoutHandler(handlers ...slog.Handler) fanoutHandler {
	return fanoutHandler{handlers: handlers}
}

func (h fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, sub := range h.handlers {
		if sub.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, sub := range h.handlers {
		if !sub.Enabled(ctx, r.Level) {
			continue
		}
		// Clone so a handler that retains the record (e.g. the capture
		// buffer) doesn't observe another handler's mutations.
		if err := sub.Handle(ctx, r.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (h fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(h.handlers))
	for i, sub := range h.handlers {
		next[i] = sub.WithAttrs(attrs)
	}
	return fanoutHandler{handlers: next}
}

func (h fanoutHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(h.handlers))
	for i, sub := range h.handlers {
		next[i] = sub.WithGroup(name)
	}
	return fanoutHandler{handlers: next}
}
