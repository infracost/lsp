package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/owenrumney/go-lsp/lsp"
	"github.com/owenrumney/go-lsp/server"
)

// progressReporter manages a single work done progress sequence.
type progressReporter struct {
	client *server.Client
	token  lsp.ProgressToken
	active bool
}

func newProgressReporter(client *server.Client) *progressReporter {
	token, _ := json.Marshal(fmt.Sprintf("infracost-scan-%d", time.Now().UnixNano()))
	return &progressReporter{
		client: client,
		token:  token,
	}
}

// Begin creates the progress token and sends a WorkDoneProgressBegin notification.
func (p *progressReporter) Begin(ctx context.Context, title string) {
	if p.client == nil {
		return
	}

	err := p.client.CreateWorkDoneProgress(ctx, &lsp.WorkDoneProgressCreateParams{
		Token: p.token,
	})
	if err != nil {
		slog.Debug("progress: client does not support workDoneProgress", "error", err)
		return
	}

	p.active = true
	p.send(ctx, lsp.WorkDoneProgressBegin{
		Kind:    "begin",
		Title:   title,
		Message: "",
	})
}

// Report sends a WorkDoneProgressReport notification.
func (p *progressReporter) Report(ctx context.Context, message string, percentage int) {
	if !p.active {
		return
	}

	p.send(ctx, lsp.WorkDoneProgressReport{
		Kind:       "report",
		Message:    message,
		Percentage: &percentage,
	})
}

// End sends a WorkDoneProgressEnd notification.
func (p *progressReporter) End(ctx context.Context, message string) {
	if !p.active {
		return
	}

	p.active = false
	p.send(ctx, lsp.WorkDoneProgressEnd{
		Kind:    "end",
		Message: message,
	})
}

func (p *progressReporter) send(ctx context.Context, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		slog.Warn("progress: failed to marshal value", "error", err)
		return
	}

	if err := p.client.Progress(ctx, &lsp.ProgressParams{
		Token: p.token,
		Value: raw,
	}); err != nil {
		slog.Warn("progress: failed to send notification", "error", err)
	}
}
