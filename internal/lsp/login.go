package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/infracost/cli/pkg/auth"
	"github.com/infracost/cli/pkg/environment"
	"github.com/infracost/lsp/internal/config"
	"github.com/owenrumney/go-lsp/lsp"
	"golang.org/x/oauth2"
)

// HandleLogin initiates the OAuth2 device authorization flow.
// It returns the verification URI and user code for the client to display,
// then polls for completion in the background.
func (s *Server) HandleLogin(_ context.Context, _ json.RawMessage) (any, error) {
	s.mu.Lock()
	if s.loginInProgress {
		s.mu.Unlock()
		return nil, fmt.Errorf("login already in progress")
	}
	s.loginInProgress = true
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)

	s.mu.Lock()
	s.loginCancel = cancel
	s.mu.Unlock()

	cfg := auth.Config{
		Environment: environment.Production,
	}
	cfg.TokenCachePath = config.TokenCachePath()
	cfg.Process()
	cfg.UseAccessTokenCache = true
	resp, err := cfg.StartDeviceFlow(ctx)
	if err != nil {
		cancel()
		s.mu.Lock()
		s.loginInProgress = false
		s.loginCancel = nil
		s.mu.Unlock()
		return nil, err
	}

	go s.pollLogin(ctx, cancel, resp)

	return struct {
		VerificationURI         string `json:"verificationUri"`
		VerificationURIComplete string `json:"verificationUriComplete"`
		UserCode                string `json:"userCode"`
	}{
		VerificationURI:         resp.VerificationURI,
		VerificationURIComplete: resp.VerificationURIComplete,
		UserCode:                resp.UserCode,
	}, nil
}

func (s *Server) pollLogin(ctx context.Context, cancel context.CancelFunc, resp *oauth2.DeviceAuthResponse) {
	defer func() {
		cancel()
		s.mu.Lock()
		s.loginInProgress = false
		s.loginCancel = nil
		s.mu.Unlock()
	}()

	cfg := auth.Config{
		Environment: environment.Production,
	}
	cfg.TokenCachePath = config.TokenCachePath()
	cfg.Process()
	cfg.UseAccessTokenCache = true
	tokenSource, err := cfg.PollDeviceFlow(ctx, resp)
	if err != nil {
		slog.Error("login: device flow failed", "error", err)
		s.showMessage(ctx, lsp.MessageTypeError, "Infracost login failed: "+err.Error())
		return
	}

	slog.Info("login: device flow complete")
	s.tokenSource.Set(tokenSource)

	// Fetch the user's profile and save it to user.json so that infracost/orgs
	// can return real org data without requiring a separate CLI login.
	if err := s.refreshUserCache(ctx); err != nil {
		slog.Warn("login: failed to refresh user cache", "error", err)
	}

	if s.client != nil {
		if err := s.client.Notify(ctx, "infracost/loginComplete", nil); err != nil {
			slog.Warn("login: failed to notify loginComplete", "error", err)
		}
	}

	if s.workspaceRoot != "" {
		go s.loadConfigAndScan() //nolint:gosec // G118: intentionally outlives request context
	}
}

// HandleLogout clears the cached token from disk and memory, then notifies the
// client to show the login screen.
func (s *Server) HandleLogout(ctx context.Context, _ json.RawMessage) (any, error) {
	cfg := &auth.Config{}
	cfg.TokenCachePath = config.TokenCachePath()
	cfg.UseAccessTokenCache = true
	if err := cfg.ClearCache(); err != nil {
		slog.Warn("logout: failed to clear token cache", "error", err)
	}
	s.tokenSource.Set(nil)
	slog.Info("logout: token cleared")

	if s.client != nil {
		if err := s.client.Notify(ctx, "infracost/logoutComplete", nil); err != nil {
			slog.Warn("logout: failed to notify logoutComplete", "error", err)
		}
	}
	return struct{}{}, nil
}

func (s *Server) showMessage(ctx context.Context, typ lsp.MessageType, msg string) {
	if s.client == nil {
		return
	}
	if err := s.client.ShowMessage(ctx, &lsp.ShowMessageParams{
		Type:    typ,
		Message: msg,
	}); err != nil {
		slog.Warn("showMessage failed", "error", err)
	}
}
