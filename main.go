package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"

	"github.com/owenrumney/go-lsp/server"

	proto "github.com/infracost/proto/gen/go/infracost/provider"

	"github.com/infracost/lsp/internal/api"
	"github.com/infracost/lsp/internal/config"
	"github.com/infracost/lsp/internal/events"
	"github.com/infracost/lsp/internal/lsp"
	"github.com/infracost/lsp/internal/plugins/parser"
	"github.com/infracost/lsp/internal/plugins/providers"
	"github.com/infracost/lsp/internal/scanner"
	"github.com/infracost/lsp/internal/update"
	"github.com/infracost/lsp/version"
)

func main() {
	if slices.Contains(os.Args[1:], "--version") || slices.Contains(os.Args[1:], "-version") {
		fmt.Println(version.Version)
		os.Exit(0)
	}

	// Interactive commands that exit immediately — these must not
	// interfere with the stdio-based LSP protocol used by IDEs.
	wantDebug := slices.Contains(os.Args[1:], "--debug")
	wantUpdate := slices.Contains(os.Args[1:], "--update")

	if wantDebug || wantUpdate {
		if wantDebug {
			runDebug()
		}
		if wantUpdate {
			if !wantDebug {
				fmt.Printf("Current version: %s\n", version.Version)
			}
			fmt.Printf("Updating...\n")
			result, err := update.Update(context.Background())
			if err != nil {
				fmt.Fprintf(os.Stderr, "Update failed: %v\n", err)
				os.Exit(1)
			}
			if !result.UpdateAvailable {
				fmt.Printf("Already up to date (v%s).\n", result.CurrentVersion)
			} else {
				fmt.Printf("Updated %s → v%s.\n", result.CurrentVersion, result.LatestVersion)
			}
		}
		os.Exit(0)
	}

	cfg := config.Load(context.Background())

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: cfg.SlogLevel,
	})))

	if err := cfg.Plugins.EnsureParser(); err != nil {
		log.Fatalf("ensuring parser plugin: %v", err)
	}

	tokenSource := api.NewTokenSource(cfg.TokenSource)
	httpClient, apiTransport := api.NewHTTPClient(tokenSource)
	eventsClient := events.NewClient(httpClient, cfg.PricingEndpoint)

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
		TokenSource:       tokenSource,
		HTTPClient:        httpClient,
		OnOrgID: func(id string) {
			apiTransport.SetOrgID(id)
			events.RegisterMetadata("orgId", id)
		},
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

	lspServer := lsp.NewServer(s, eventsClient, tokenSource)

	var opts []server.Option
	slog.Info("debug UI config", "INFRACOST_DEBUG_UI", cfg.DebugUI) //nolint:gosec
	if cfg.DebugUI != "" {
		// check if the debug UI port is available before starting the server
		// if its bound, we're going to log it and move on
		if err := checkPortAvailable(cfg.DebugUI); err != nil {
			slog.Error("debug UI port is not available", "port", cfg.DebugUI, "error", err)
		} else {
			opts = append(opts, server.WithDebugUI(cfg.DebugUI))
			opts = append(opts, server.WithLogger(slog.Default()))
		}
	}
	srv := server.NewServer(lspServer, opts...)
	lspServer.SetServer(srv)

	srv.HandleMethod("infracost/resourceDetails", lspServer.HandleResourceDetails)
	srv.HandleMethod("infracost/fileSummary", lspServer.HandleFileSummary)
	srv.HandleMethod("infracost/status", lspServer.HandleStatus)
	srv.HandleMethod("infracost/login", lspServer.HandleLogin)
	srv.HandleMethod("infracost/logout", lspServer.HandleLogout)
	srv.HandleMethod("infracost/update", lspServer.HandleUpdate)
	srv.HandleMethod("infracost/orgs", lspServer.HandleOrgs)
	srv.HandleMethod("infracost/selectOrg", lspServer.HandleSelectOrg)
	srv.HandleMethod("infracost/workspaceSummary", lspServer.HandleWorkspaceSummary)

	slog.Info("listening on stdio")
	if err := srv.Run(context.Background(), server.RunStdio()); err != nil {
		log.Printf("server error: %v", err)
		return
	}
	slog.Info("server stopped")
}

func checkPortAvailable(hostPort string) error {
	ln, err := net.Listen("tcp", hostPort)
	if err != nil {
		return err
	}
	_ = ln.Close()
	return nil
}

func runDebug() {
	fmt.Printf("infracost-ls %s\n", version.Version)
	fmt.Printf("  go:       %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	execPath, err := os.Executable()
	if err == nil {
		execPath, _ = filepath.EvalSymlinks(execPath)
		fmt.Printf("  bin:      %s\n", execPath)
	}

	cfg := config.Load(context.Background())
	fmt.Printf("  auth:     %v\n", cfg.TokenSource != nil)

	fmt.Printf("\nEndpoints:\n")
	fmt.Printf("  pricing:   %s\n", cfg.PricingEndpoint)
	fmt.Printf("  dashboard: %s\n", cfg.DashboardEndpoint)
	fmt.Printf("  plugins:   %s\n", cfg.Plugins.ManifestURL)
	fmt.Printf("  releases:  https://api.github.com/repos/infracost/lsp/releases/latest\n")

	fmt.Printf("\nChecking for updates...\n")
	result, err := update.Check(context.Background())
	switch {
	case err != nil:
		fmt.Printf("  update check failed: %v\n", err)
	case result.UpdateAvailable:
		fmt.Printf("  update available: v%s → v%s\n", result.CurrentVersion, result.LatestVersion)
	default:
		fmt.Printf("  up to date (v%s)\n", result.CurrentVersion)
	}
}
