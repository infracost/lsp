package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"slices"

	"github.com/owenrumney/go-lsp/server"
	"golang.org/x/oauth2"

	proto "github.com/infracost/proto/gen/go/infracost/provider"

	"github.com/infracost/lsp/internal/config"
	"github.com/infracost/lsp/internal/events"
	"github.com/infracost/lsp/internal/lsp"
	"github.com/infracost/lsp/internal/plugins/parser"
	"github.com/infracost/lsp/internal/plugins/providers"
	"github.com/infracost/lsp/internal/scanner"
	"github.com/infracost/lsp/version"
)

func main() {
	if slices.Contains(os.Args[1:], "--version") || slices.Contains(os.Args[1:], "-version") {
		fmt.Println(version.Version)
		os.Exit(0)
	}

	cfg := config.Load(context.Background())

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: cfg.SlogLevel,
	})))

	if err := cfg.Plugins.EnsureParser(); err != nil {
		log.Fatalf("ensuring parser plugin: %v", err)
	}

	eventsClient := events.NewClient(http.DefaultClient, cfg.PricingEndpoint)

	defer func() {
		if r := recover(); r != nil {
			eventsClient.Push(context.Background(), "infracost-error", "error", r, "stacktrace", string(debug.Stack()))
			_, _ = fmt.Fprintf(os.Stderr, "panic: %v\n\n%s\n", r, debug.Stack())
			os.Exit(1)
		}
	}()

	parserClient := parser.PluginClient{
		Plugin:  cfg.Plugins.Parser.Plugin,
		Version: cfg.Plugins.Parser.Version,
	}
	providerClient := providers.PluginClient{
		AWS:           cfg.Plugins.Providers.AWS,
		Google:        cfg.Plugins.Providers.Google,
		Azure:         cfg.Plugins.Providers.Azure,
		AWSVersion:    cfg.Plugins.Providers.AWSVersion,
		GoogleVersion: cfg.Plugins.Providers.GoogleVersion,
		AzureVersion:  cfg.Plugins.Providers.AzureVersion,
	}

	slog.Info("starting infracost-ls",
		"parser_plugin", parserClient.Plugin,
		"currency", cfg.Currency,
		"pricing_endpoint", cfg.PricingEndpoint,
		"has_token_source", cfg.TokenSource != nil,
		"dashboard_endpoint", cfg.DashboardEndpoint,
		"hclog_level", cfg.LogLevel.String(),
	)

	s := &scanner.Scanner{
		Parser:            &parserClient,
		Provider:          &providerClient,
		LogLevel:          cfg.LogLevel,
		Currency:          cfg.Currency,
		PricingEndpoint:   cfg.PricingEndpoint,
		DashboardEndpoint: cfg.DashboardEndpoint,
		EnsureProvider: func(p proto.Provider) error {
			if err := cfg.Plugins.EnsureProvider(p); err != nil {
				return err
			}
			providerClient.AWS = cfg.Plugins.Providers.AWS
			providerClient.Google = cfg.Plugins.Providers.Google
			providerClient.Azure = cfg.Plugins.Providers.Azure
			return nil
		},
	}
	s.Init()
	if cfg.TokenSource != nil {
		s.SetTokenSource(cfg.TokenSource)
		eventsClient.SetTokenSource(oauthTokenAdapter{cfg.TokenSource})
	}

	lspServer := lsp.NewServer(s, eventsClient)

	var opts []server.Option
	slog.Info("debug UI config", "INFRACOST_DEBUG_UI", cfg.DebugUI) //nolint:gosec
	if cfg.DebugUI != "" {
		// check if the debug UI port is available before starting the server
		// if its bound, we're going to log it and move on
		if err := checkPortAvailable(cfg.DebugUI); err != nil {
			slog.Error("debug UI port is not available", "port", cfg.DebugUI, "error", err)
		} else {
			opts = append(opts, server.WithDebugUI(cfg.DebugUI))
		}
	}
	srv := server.NewServer(lspServer, opts...)
	lspServer.SetServer(srv)

	srv.HandleMethod("infracost/resourceDetails", lspServer.HandleResourceDetails)
	srv.HandleMethod("infracost/login", lspServer.HandleLogin)

	slog.Info("listening on stdio")
	if err := srv.Run(context.Background(), server.RunStdio()); err != nil {
		log.Printf("server error: %v", err)
		return
	}
	slog.Info("server stopped")
}

type oauthTokenAdapter struct {
	ts oauth2.TokenSource
}

func (a oauthTokenAdapter) Token() (string, error) {
	t, err := a.ts.Token()
	if err != nil {
		return "", err
	}
	return t.AccessToken, nil
}

func checkPortAvailable(hostPort string) error {
	ln, err := net.Listen("tcp", hostPort)
	if err != nil {
		return err
	}
	_ = ln.Close()
	return nil
}
