package lsp

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/infracost/lsp/internal/update"
)

// HandleUpdate downloads and installs the latest version of the LSP binary.
func (s *Server) HandleUpdate(_ context.Context, _ json.RawMessage) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	result, err := update.Update(ctx)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// checkForUpdate queries GitHub for the latest release and notifies the client
// if an update is available. Called asynchronously during initialization.
func (s *Server) checkForUpdate() {
	ctx, cancel := context.WithTimeout(context.Background(), update.UpdateCheckTimeout)
	defer cancel()

	result, err := update.Check(ctx)
	if err != nil {
		slog.Warn("update check failed", "error", err)
		return
	}

	if !result.UpdateAvailable {
		slog.Info("LSP is up to date", "version", result.CurrentVersion)
		return
	}

	slog.Info("update available", "current", result.CurrentVersion, "latest", result.LatestVersion)
	if s.client != nil {
		if err := s.client.Notify(ctx, "infracost/updateAvailable", result); err != nil {
			slog.Warn("failed to send update notification", "error", err)
		}
	}
}
