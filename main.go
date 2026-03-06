package main

import (
	"context"
	"log"
	"log/slog"
	"os"

	"github.com/owenrumney/go-lsp/server"

	proto "github.com/infracost/proto/gen/go/infracost/provider"

	"github.com/infracost/lsp/internal/config"
	"github.com/infracost/lsp/internal/lsp"
	"github.com/infracost/lsp/internal/plugins/parser"
	"github.com/infracost/lsp/internal/plugins/providers"
	"github.com/infracost/lsp/internal/scanner"
)

func main() {
	cfg := config.Load(context.Background())

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: cfg.SlogLevel,
	})))

	if err := cfg.Plugins.EnsureParser(); err != nil {
		log.Fatalf("ensuring parser plugin: %v", err)
	}

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
	}

	lspServer := lsp.NewServer(s)

	var opts []server.Option
	slog.Info("debug UI config", "INFRACOST_DEBUG_UI", cfg.DebugUI) //nolint:gosec
	if cfg.DebugUI != "" {
		opts = append(opts, server.WithDebugUI(cfg.DebugUI))
	}
	srv := server.NewServer(lspServer, opts...)
	lspServer.SetServer(srv)

	srv.HandleMethod("infracost/resourceDetails", lspServer.HandleResourceDetails)
	srv.HandleMethod("infracost/login", lspServer.HandleLogin)

	slog.Info("listening on stdio")
	if err := srv.Run(context.Background(), server.RunStdio()); err != nil {
		log.Fatalf("server error: %v", err)
	}
	slog.Info("server stopped")
}
