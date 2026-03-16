package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/owenrumney/go-lsp/lsp"
	"github.com/owenrumney/go-lsp/server"

	repoconfig "github.com/infracost/config"

	"github.com/infracost/lsp/internal/events"
	"github.com/infracost/lsp/internal/ignore"
	"github.com/infracost/lsp/internal/scanner"
	"github.com/infracost/lsp/version"
)

// Settings holds client-provided configuration synced via workspace/didChangeConfiguration.
type Settings struct {
	RunParamsCacheTTLSeconds int `json:"runParamsCacheTTLSeconds"`
}

const defaultRunParamsCacheTTLSeconds = 300

// isWindows controls whether uriToPath applies Windows-specific path
// transforms (drive-letter stripping, separator conversion). It is a var
// so tests can override it to exercise Windows codepaths on any OS.
var isWindows = runtime.GOOS == "windows"

type Server struct {
	scanner *scanner.Scanner
	events  events.Client
	client  *server.Client
	srv     *server.Server
	ignores *ignore.Store

	mu             sync.RWMutex
	settings       Settings
	config         *repoconfig.Config
	projectResults map[string]*scanner.ScanResult // project name → result

	// filesWithDiagnostics tracks URIs that currently have published diagnostics.
	// Used to clear diagnostics when violations are resolved.
	filesWithDiagnostics map[string]struct{}

	// scanningProjects tracks which projects are currently being scanned.
	scanningProjects map[string]struct{}

	// scanCancels holds cancel functions for in-flight scans, keyed by project name.
	scanCancels map[string]context.CancelFunc

	// scanTimers holds debounce timers for pending scan requests, keyed by project name.
	scanTimers map[string]*time.Timer

	// workspaceRoot is the root directory from the client's initialize request.
	workspaceRoot string

	// clientSupportsCodeLens is true when the client advertises CodeLens support.
	// When true, inlay hints are skipped to avoid duplication.
	clientSupportsCodeLens bool

	// loginInProgress is true while a device authorization flow is running.
	loginInProgress bool
	loginCancel     context.CancelFunc
}

func NewServer(s *scanner.Scanner, eventsClient events.Client) *Server {
	ignores, err := ignore.NewStore()
	if err != nil {
		slog.Warn("failed to load ignore store", "error", err)
	}

	return &Server{
		scanner:              s,
		events:               eventsClient,
		ignores:              ignores,
		projectResults:       make(map[string]*scanner.ScanResult),
		filesWithDiagnostics: make(map[string]struct{}),
		scanningProjects:     make(map[string]struct{}),
		scanCancels:          make(map[string]context.CancelFunc),
		scanTimers:           make(map[string]*time.Timer),
	}
}

// SetClient implements server.ClientHandler. It is called by the go-lsp
// server after the connection is established, providing access to
// server→client communication.
func (s *Server) SetClient(c *server.Client) {
	s.client = c
}

func (s *Server) SetServer(srv *server.Server) {
	s.srv = srv
}

// Initialize implements server.LifecycleHandler.
func (s *Server) Initialize(_ context.Context, params *lsp.InitializeParams) (*lsp.InitializeResult, error) {
	if h := s.srv.DebugHandler(); h != nil {
		slog.SetDefault(slog.New(h))
	}

	rootURI := ""
	if params.RootURI != nil {
		rootURI = string(*params.RootURI)
	}
	s.workspaceRoot = uriToPath(rootURI)
	slog.Info("initialize",
		"workspace_root", s.workspaceRoot,
		"client", params.ClientInfo,
	)

	s.registerClientMetadata(params)

	if params.Capabilities.TextDocument != nil && params.Capabilities.TextDocument.CodeLens != nil {
		s.clientSupportsCodeLens = true
	}

	s.scanner.SetRunParamsTTL(time.Duration(defaultRunParamsCacheTTLSeconds) * time.Second)

	if s.workspaceRoot != "" {
		go s.loadConfigAndScan() //nolint:gosec // G118: intentionally outlives request context
	}

	enabled := true

	return &lsp.InitializeResult{
		Capabilities: lsp.ServerCapabilities{
			TextDocumentSync: &lsp.TextDocumentSyncOptions{
				OpenClose: &enabled,
				Change:    lsp.SyncIncremental,
				Save:      &lsp.SaveOptions{IncludeText: &enabled},
			},
			CodeLensProvider: &lsp.CodeLensOptions{},
			CodeActionProvider: &lsp.CodeActionOptions{
				CodeActionKinds: []lsp.CodeActionKind{
					lsp.CodeActionQuickFix,
				},
			},
			HoverProvider: &enabled,
			Workspace: &lsp.ServerWorkspaceCapabilities{
				WorkspaceFolders: &lsp.WorkspaceFoldersServerCapabilities{
					Supported:           &enabled,
					ChangeNotifications: &enabled,
				},
			},
			ExecuteCommandProvider: &lsp.ExecuteCommandOptions{
				Commands: []string{"infracost.dismissDiagnostic"},
			},
		},
		ServerInfo: &lsp.ServerInfo{
			Name:    "infracost-ls",
			Version: version.Version,
		},
	}, nil
}

// registerClientMetadata updates event metadata with client information from the
// initialize request. The IDE name and extension version (from initializationOptions)
// take priority, with "infracost-ls" and the LSP version as fallbacks.
func (s *Server) registerClientMetadata(params *lsp.InitializeParams) {
	if params.ClientInfo != nil {
		if params.ClientInfo.Name != "" {
			events.RegisterMetadata("caller", params.ClientInfo.Name)
		}
	}

	var initOpts struct {
		ExtensionVersion string `json:"extensionVersion"`
	}
	if len(params.InitializationOptions) > 0 {
		if err := json.Unmarshal(params.InitializationOptions, &initOpts); err != nil {
			slog.Warn("failed to parse initializationOptions", "error", err)
		}
	}
	if initOpts.ExtensionVersion != "" {
		events.RegisterMetadata("version", initOpts.ExtensionVersion)
	}
}

// ExecuteCommand implements server.ExecuteCommandHandler.
func (s *Server) ExecuteCommand(_ context.Context, params *lsp.ExecuteCommandParams) (any, error) {
	if params.Command == "infracost.dismissDiagnostic" {
		return json.RawMessage("null"), s.handleDismissDiagnostic(params.Arguments)
	}
	return json.RawMessage("null"), nil
}

func (s *Server) handleDismissDiagnostic(args []json.RawMessage) error {
	if len(args) != 3 {
		return fmt.Errorf("dismissDiagnostic: expected 3 arguments, got %d", len(args))
	}

	var absPath, resource, slug string
	if err := json.Unmarshal(args[0], &absPath); err != nil {
		return fmt.Errorf("dismissDiagnostic: invalid path argument: %w", err)
	}
	if err := json.Unmarshal(args[1], &resource); err != nil {
		return fmt.Errorf("dismissDiagnostic: invalid resource argument: %w", err)
	}
	if err := json.Unmarshal(args[2], &slug); err != nil {
		return fmt.Errorf("dismissDiagnostic: invalid slug argument: %w", err)
	}

	slog.Info("dismissDiagnostic", "path", absPath, "resource", resource, "slug", slug)

	if err := s.ignores.Add(absPath, resource, slug); err != nil {
		return fmt.Errorf("dismissDiagnostic: failed to add ignore: %w", err)
	}

	s.publishDiagnostics()
	return nil
}

// Shutdown implements server.LifecycleHandler.
func (s *Server) Shutdown(_ context.Context) error {
	slog.Info("shutdown requested")

	s.mu.Lock()
	for name, t := range s.scanTimers {
		t.Stop()
		delete(s.scanTimers, name)
	}
	for name, cancel := range s.scanCancels {
		cancel()
		delete(s.scanCancels, name)
	}
	s.mu.Unlock()

	s.scanner.Close()
	return nil
}

// DidChangeWorkspaceFolders implements server.WorkspaceFoldersHandler.
func (s *Server) DidChangeWorkspaceFolders(_ context.Context, params *lsp.DidChangeWorkspaceFoldersParams) error {
	slog.Info("didChangeWorkspaceFolders",
		"added", len(params.Event.Added),
		"removed", len(params.Event.Removed),
	)

	if len(params.Event.Added) == 0 {
		return nil
	}

	newRoot := uriToPath(string(params.Event.Added[0].URI))
	slog.Info("workspace root changed", "old", s.workspaceRoot, "new", newRoot)

	s.resetState()
	s.workspaceRoot = newRoot
	go s.loadConfigAndScan() //nolint:gosec // G118: intentionally outlives request context

	return nil
}

// DidChangeConfiguration implements server.DidChangeConfigurationHandler.
func (s *Server) DidChangeConfiguration(_ context.Context, params *lsp.DidChangeConfigurationParams) error {
	raw, err := json.Marshal(params.Settings)
	if err != nil {
		slog.Warn("didChangeConfiguration: failed to marshal settings", "error", err)
		return nil
	}

	var settings Settings
	if err := json.Unmarshal(raw, &settings); err != nil {
		slog.Warn("didChangeConfiguration: failed to unmarshal settings", "error", err)
		return nil
	}

	s.mu.Lock()
	s.settings = settings
	s.mu.Unlock()

	ttl := settings.RunParamsCacheTTLSeconds
	if ttl <= 0 {
		ttl = defaultRunParamsCacheTTLSeconds
	}
	s.scanner.SetRunParamsTTL(time.Duration(ttl) * time.Second)

	slog.Info("didChangeConfiguration", "settings", settings)
	return nil
}

// DidOpen implements server.TextDocumentSyncHandler.
func (s *Server) DidOpen(_ context.Context, params *lsp.DidOpenTextDocumentParams) error {
	uri := string(params.TextDocument.URI)
	slog.Debug("didOpen", "uri", uri, "language", params.TextDocument.LanguageID)

	if !isSupportedFile(uri) {
		return nil
	}

	if !s.scanner.HasTokenSource() {
		s.publishAuthDiagnostic(uri)
	}

	s.scheduleAnalyze(uri)
	return nil
}

// DidChange implements server.TextDocumentSyncHandler.
func (s *Server) DidChange(_ context.Context, _ *lsp.DidChangeTextDocumentParams) error {
	return nil
}

// DidClose implements server.TextDocumentSyncHandler.
func (s *Server) DidClose(_ context.Context, _ *lsp.DidCloseTextDocumentParams) error {
	return nil
}

// DidSave implements server.TextDocumentSaveHandler.
func (s *Server) DidSave(_ context.Context, params *lsp.DidSaveTextDocumentParams) error {
	uri := string(params.TextDocument.URI)
	slog.Debug("didSave", "uri", uri)

	if !isSupportedFile(uri) {
		return nil
	}
	s.scheduleAnalyze(uri)
	return nil
}

// loadConfigAndScan loads the workspace config and runs an initial scan of all projects.
func (s *Server) loadConfigAndScan() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	progress := newProgressReporter(s.client)
	progress.Begin(ctx, "Scanning workspace")
	defer progress.End(ctx, "Scan complete")

	slog.Info("loadConfigAndScan: loading config", "dir", s.workspaceRoot)
	cfg, err := scanner.LoadConfig(s.workspaceRoot)
	if err != nil {
		slog.Error("loadConfigAndScan: failed to load config", "error", err)
		progress.End(ctx, "Failed to load config")
		return
	}
	s.setConfig(cfg)
	slog.Info("loadConfigAndScan: config loaded", "projects", len(cfg.Projects))

	totalResources := 0
	totalViolations := 0
	totalTagViolations := 0

	for i, project := range cfg.Projects {
		pct := (i * 100) / len(cfg.Projects)
		progress.Report(ctx, fmt.Sprintf("Scanning %s...", project.Name), pct)

		s.setScanningProject(project.Name, true)

		start := time.Now()
		result, err := s.scanner.ScanProject(ctx, s.workspaceRoot, cfg, project)
		elapsed := time.Since(start)

		s.setScanningProject(project.Name, false)

		if err != nil {
			slog.Error("loadConfigAndScan: project scan failed", "name", project.Name, "error", err, "elapsed", elapsed)
			continue
		}

		slog.Info("loadConfigAndScan: project scanned",
			"name", project.Name,
			"resources", len(result.Resources),
			"violations", len(result.Violations),
			"tag_violations", len(result.TagViolations),
			"elapsed", elapsed,
		)

		s.trackRun(ctx, result, elapsed)

		totalResources += len(result.Resources)
		totalViolations += len(result.Violations)
		totalTagViolations += len(result.TagViolations)

		s.setProjectResult(project.Name, result)
		s.refreshCodeLenses()
		s.refreshInlayHints()
		s.publishDiagnostics()
		s.sendScanComplete()
	}

	progress.End(ctx, fmt.Sprintf("Scan complete — %d resources, %d violations, %d tag issues", totalResources, totalViolations, totalTagViolations))
}

// refreshInlayHints asks the client to re-request inlay hints.
func (s *Server) refreshInlayHints() {
	if s.client == nil {
		slog.Warn("refreshInlayHints: client not available yet")
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		slog.Debug("refreshInlayHints: sending workspace/inlayHint/refresh")
		if err := s.client.InlayHintRefresh(ctx); err != nil {
			slog.Warn("refreshInlayHints: failed", "error", err)
			return
		}
		slog.Debug("refreshInlayHints: client acknowledged")
	}()
}

// sendScanComplete sends an infracost/scanComplete notification to the client.
// The client uses this to trigger UI refresh (code vision, sidebar) without polling.
func (s *Server) sendScanComplete() {
	if s.client == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.client.Notify(ctx, "infracost/scanComplete", nil); err != nil {
			slog.Warn("sendScanComplete: failed", "error", err)
		}
	}()
}

// refreshCodeLenses asks the client to re-request code lenses.
func (s *Server) refreshCodeLenses() {
	if s.client == nil {
		slog.Warn("refreshCodeLenses: client not available yet")
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		slog.Debug("refreshCodeLenses: sending workspace/codeLens/refresh")
		if err := s.client.CodeLensRefresh(ctx); err != nil {
			slog.Warn("refreshCodeLenses: failed", "error", err)
			return
		}
		slog.Debug("refreshCodeLenses: client acknowledged")
	}()
}

func (s *Server) getMergedResult() *scanner.ScanResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	merged := &scanner.ScanResult{}
	for _, r := range s.projectResults {
		if r == nil {
			continue
		}
		merged.Resources = append(merged.Resources, r.Resources...)
		merged.ModuleCosts = append(merged.ModuleCosts, r.ModuleCosts...)
		merged.Violations = append(merged.Violations, r.Violations...)
		merged.TagViolations = append(merged.TagViolations, r.TagViolations...)
		merged.Errors = append(merged.Errors, r.Errors...)
	}
	return merged
}

func (s *Server) setProjectResult(name string, result *scanner.ScanResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projectResults[name] = result
}

func (s *Server) setScanningProject(name string, scanning bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if scanning {
		s.scanningProjects[name] = struct{}{}
	} else {
		delete(s.scanningProjects, name)
	}
}

func (s *Server) isScanning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.scanningProjects) > 0
}

func (s *Server) isScanningProject(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.scanningProjects[name]
	return ok
}

// resetState cancels all in-flight scans, stops debounce timers, and clears
// cached results. Used when the workspace root changes.
func (s *Server) resetState() {
	s.mu.Lock()
	for name, t := range s.scanTimers {
		t.Stop()
		delete(s.scanTimers, name)
	}
	for name, cancel := range s.scanCancels {
		cancel()
		delete(s.scanCancels, name)
	}
	s.config = nil
	s.projectResults = make(map[string]*scanner.ScanResult)
	s.scanningProjects = make(map[string]struct{})
	s.filesWithDiagnostics = make(map[string]struct{})
	s.mu.Unlock()
}

func (s *Server) getConfig() *repoconfig.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

func (s *Server) setConfig(cfg *repoconfig.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config = cfg
}

// findProjectForFile returns the project whose path is a prefix of filePath.
// If multiple projects match (nested paths), the most specific one wins.
func findProjectForFile(cfg *repoconfig.Config, rootDir, filePath string) *repoconfig.Project {
	var best *repoconfig.Project
	bestLen := 0

	for _, p := range cfg.Projects {
		projDir := filepath.Clean(filepath.Join(rootDir, p.Path))
		// Check if the file is under this project's directory.
		if strings.HasPrefix(filePath, projDir+string(filepath.Separator)) || filePath == projDir {
			if len(projDir) > bestLen {
				best = p
				bestLen = len(projDir)
			}
		}
	}
	return best
}

func uriToPath(uri string) string {
	if strings.HasPrefix(uri, "file://") {
		u, err := url.Parse(uri)
		if err == nil {
			path := u.Path
			if isWindows {
				// On Windows, url.Parse returns "/c:/path" for "file:///c:/path"
				// We need to remove the leading slash for Windows drive letters.
				if len(path) >= 3 && path[0] == '/' && path[2] == ':' {
					path = path[1:]
				}
				// Normalize drive letter to uppercase to match filepath.Abs output.
				// VSCode sometimes sends lowercase drive letters (e.g. c%3A → c:).
				if len(path) >= 2 && path[1] == ':' {
					path = strings.ToUpper(path[:1]) + path[1:]
				}
				// Convert forward slashes to backslashes on Windows so path
				// comparisons using filepath.Join/filepath.Clean work correctly.
				if len(path) >= 2 && path[1] == ':' {
					path = strings.ReplaceAll(path, "/", "\\")
				}
			}
			return path
		}
	}
	// Handle POSIX-style Windows paths sent without a file:// prefix, e.g. "/c:/Users/..."
	if isWindows && len(uri) >= 3 && uri[0] == '/' && uri[2] == ':' {
		return strings.ReplaceAll(uri[1:], "/", "\\")
	}
	return uri
}

func pathToURI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	// Convert Windows paths to proper URI format
	// Replace backslashes with forward slashes for URI
	abs = strings.ReplaceAll(abs, "\\", "/")
	// Ensure Windows drive letters are properly formatted in URI
	if len(abs) >= 2 && abs[1] == ':' {
		abs = "/" + abs
	}
	u := &url.URL{Scheme: "file", Path: abs}
	return u.String()
}

func isSupportedFile(uri string) bool {
	if strings.HasSuffix(uri, ".tf") || strings.HasSuffix(uri, ".hcl") {
		return true
	}

	lower := strings.ToLower(uri)
	if strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".json") {
		return isCloudFormationFile(uriToPath(uri))
	}
	return false
}

func isCloudFormationFile(path string) bool {
	cleanPath := filepath.Clean(path)
	f, err := os.Open(cleanPath) //nolint:gosec // path is from trusted LSP workspace URIs
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Resources:") || strings.HasPrefix(line, "AWSTemplateFormatVersion:") ||
			strings.Contains(line, `"Resources"`) || strings.Contains(line, `"AWSTemplateFormatVersion"`) {
			return true
		}
	}
	return false
}

func ptrTo[T any](v T) *T {
	return &v
}
