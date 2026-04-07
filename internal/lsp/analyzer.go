package lsp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/infracost/lsp/internal/scanner"
)

const scanDebounce = 300 * time.Millisecond

// scheduleAnalyze debounces scan requests per project. Rapid saves within
// the debounce window are coalesced into a single scan. A new scan cancels
// any in-flight scan for the same project.
func (s *Server) scheduleAnalyze(uri string) {
	filePath := uriToPath(uri)

	cfg := s.getConfig()
	if cfg == nil {
		slog.Info("scheduleAnalyze: config not loaded, running full scan", "uri", uri)
		go s.analyzeFullScan(uri)
		return
	}

	project := findProjectForFile(cfg, s.workspaceRoot, filePath)
	if project == nil {
		slog.Debug("scheduleAnalyze: file not in any known project, skipping", "uri", uri)
		return
	}

	projectName := project.Name

	// Don't queue a scan if the project is already being scanned (e.g., during
	// initial loadConfigAndScan). Concurrent scans of the same project cause
	// provider plugin conflicts and timeouts.
	if s.isScanningProject(projectName) {
		slog.Debug("scheduleAnalyze: project already scanning, skipping", "project", projectName, "uri", uri)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if t, ok := s.scanTimers[projectName]; ok {
		t.Stop()
	}

	s.scanTimers[projectName] = time.AfterFunc(scanDebounce, func() {
		s.mu.Lock()
		delete(s.scanTimers, projectName)
		if cancel, ok := s.scanCancels[projectName]; ok {
			cancel()
			delete(s.scanCancels, projectName)
		}
		ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec // G118: intentionally outlives request; cancel stored in scanCancels
		s.scanCancels[projectName] = cancel
		s.mu.Unlock()

		s.analyze(ctx, uri)

		s.mu.Lock()
		delete(s.scanCancels, projectName)
		s.mu.Unlock()
	})
}

func (s *Server) analyze(ctx context.Context, uri string) {
	filePath := uriToPath(uri)

	cfg := s.getConfig()
	if cfg == nil {
		slog.Info("analyze: config not loaded, running full scan", "uri", uri)
		s.analyzeFullScan(uri)
		return
	}

	project := findProjectForFile(cfg, s.workspaceRoot, filePath)
	if project == nil {
		slog.Debug("analyze: file not in any known project, skipping", "uri", uri)
		return
	}

	slog.Info("analyze: scanning project", "project", project.Name)

	s.setScanningProject(project.Name, true)
	s.refreshCodeLenses()
	s.refreshInlayHints()

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	progress := newProgressReporter(s.client)
	progress.Begin(ctx, fmt.Sprintf("Scanning %s...", project.Name))

	start := time.Now()
	result, err := s.scanner.ScanProject(ctx, s.workspaceRoot, cfg, project)
	elapsed := time.Since(start)

	s.setScanningProject(project.Name, false)

	if err != nil {
		if ctx.Err() != nil {
			slog.Info("analyze: scan cancelled", "project", project.Name, "elapsed", elapsed)
			progress.End(ctx, "Scan cancelled")
			s.refreshCodeLenses()
			s.refreshInlayHints()
			return
		}
		slog.Error("analyze: scan failed", "project", project.Name, "error", err, "elapsed", elapsed)
		progress.End(ctx, fmt.Sprintf("Scan failed: %s", err))
		s.refreshCodeLenses()
		s.refreshInlayHints()
		return
	}

	if ctx.Err() != nil {
		slog.Info("analyze: scan cancelled after completion", "project", project.Name)
		return
	}

	slog.Info("analyze: scan complete",
		"project", project.Name,
		"resources", len(result.Resources),
		"violations", len(result.Violations),
		"errors", len(result.Errors),
		"elapsed", elapsed,
	)

	s.trackRun(ctx, result, elapsed)

	for _, e := range result.Errors {
		slog.Warn("analyze: scan error", "error", e)
	}

	for _, r := range result.Resources {
		slog.Debug("analyze: resource",
			"name", r.Name,
			"file", r.Filename,
			"line", r.StartLine,
			"cost", scanner.FormatCost(r.MonthlyCost),
		)
	}

	s.trackDiff(ctx, project.Name, result)
	s.setProjectResult(project.Name, result)
	s.refreshCodeLenses()
	s.refreshInlayHints()
	s.publishDiagnostics()
	s.sendScanComplete()

	progress.End(ctx, fmt.Sprintf("%d resources, %d violations", len(result.Resources), len(result.Violations)))
}

// analyzeFullScan is the fallback when config hasn't been loaded yet.
// It loads config, caches it, and scans all projects.
func (s *Server) analyzeFullScan(uri string) {
	dir := s.workspaceRoot
	if dir == "" {
		path := uriToPath(uri)
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			dir = path
		} else {
			dir = filepath.Dir(path)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	progress := newProgressReporter(s.client)
	progress.Begin(ctx, "Scanning workspace")
	defer progress.End(ctx, "Scan complete")

	cfg, err := scanner.LoadConfig(dir)
	if err != nil {
		slog.Error("analyzeFullScan: failed to load config", "error", err)
		progress.End(ctx, "Failed to load config")
		return
	}
	s.setConfig(cfg)

	totalResources := 0
	totalViolations := 0

	for i, project := range cfg.Projects {
		pct := (i * 100) / len(cfg.Projects)
		progress.Report(ctx, fmt.Sprintf("Scanning %s...", project.Name), pct)

		s.setScanningProject(project.Name, true)

		start := time.Now()
		result, err := s.scanner.ScanProject(ctx, dir, cfg, project)
		elapsed := time.Since(start)

		s.setScanningProject(project.Name, false)

		if err != nil {
			slog.Error("analyzeFullScan: project scan failed", "name", project.Name, "error", err, "elapsed", elapsed)
			continue
		}

		slog.Info("analyzeFullScan: project scanned",
			"name", project.Name,
			"resources", len(result.Resources),
			"violations", len(result.Violations),
			"elapsed", elapsed,
		)

		totalResources += len(result.Resources)
		totalViolations += len(result.Violations)

		s.trackDiff(ctx, project.Name, result)
		s.setProjectResult(project.Name, result)
		s.refreshCodeLenses()
		s.refreshInlayHints()
		s.publishDiagnostics()
	}

	progress.End(ctx, fmt.Sprintf("Scan complete — %d resources, %d violations", totalResources, totalViolations))
}

func (s *Server) trackRun(ctx context.Context, result *scanner.ScanResult, elapsed time.Duration) {
	if s.events == nil {
		return
	}

	var totalResources, totalSupported, totalNoPrice, totalUnsupported int
	supportedCounts := make(map[string]int)
	unsupportedCounts := make(map[string]int)

	for _, r := range result.Resources {
		totalResources++
		switch {
		case !r.IsSupported:
			totalUnsupported++
			unsupportedCounts[r.Type]++
		case r.IsFree:
			totalNoPrice++
		default:
			totalSupported++
			supportedCounts[r.Type]++
		}
	}

	go s.events.Push(context.WithoutCancel(ctx), "infracost-run",
		"runSeconds", elapsed.Seconds(),
		"totalResources", totalResources,
		"totalSupportedResources", totalSupported,
		"totalNoPriceResources", totalNoPrice,
		"totalUnsupportedResources", totalUnsupported,
		"supportedResourceCounts", supportedCounts,
		"unsupportedResourceCounts", unsupportedCounts,
	)
}

// trackDiff compares the new scan result against the previous result for the
// same project and fires a "cloud-issue-fixed" event for every violation that
// was present before but is no longer present.
func (s *Server) trackDiff(ctx context.Context, projectName string, result *scanner.ScanResult) {
	if s.events == nil {
		return
	}

	prev := s.getProjectResult(projectName)
	if prev == nil {
		return
	}

	slog.Debug("trackDiff: comparing results",
		"project", projectName,
		"prevFinops", len(prev.Violations),
		"newFinops", len(result.Violations),
		"prevTags", len(prev.TagViolations),
		"newTags", len(result.TagViolations),
	)

	detachedCtx := context.WithoutCancel(ctx)

	// Finops violations: keyed by (policySlug, address).
	currentFinops := make(map[[2]string]struct{}, len(result.Violations))
	for _, v := range result.Violations {
		currentFinops[[2]string{v.PolicySlug, v.Address}] = struct{}{}
	}
	for _, v := range prev.Violations {
		if _, ok := currentFinops[[2]string{v.PolicySlug, v.Address}]; ok {
			continue
		}
		slog.Debug("trackDiff: finops issue fixed",
			"policySlug", v.PolicySlug,
			"address", v.Address,
		)
		go s.events.Push(detachedCtx, "cloud-issue-fixed",
			"policyId", v.PolicyID,
			"policySlug", v.PolicySlug,
			"type", "finops-policy",
			"projectName", projectName,
			"resourceAddress", v.Address,
			"pullRequestId", "",
			"autoFixPullRequest", false,
		)
	}

	// Tag violations: keyed by (policyID, address).
	currentTags := make(map[[2]string]struct{}, len(result.TagViolations))
	for _, v := range result.TagViolations {
		currentTags[[2]string{v.PolicyID, v.Address}] = struct{}{}
	}
	for _, v := range prev.TagViolations {
		if _, ok := currentTags[[2]string{v.PolicyID, v.Address}]; ok {
			continue
		}
		slog.Debug("trackDiff: tag issue fixed",
			"policyID", v.PolicyID,
			"address", v.Address,
		)
		go s.events.Push(detachedCtx, "cloud-issue-fixed",
			"policyId", v.PolicyID,
			"type", "tag-policy",
			"projectName", projectName,
			"resourceAddress", v.Address,
			"pullRequestId", "",
			"autoFixPullRequest", false,
		)
	}
}

func safeLineToLSP(line int64) int {
	if line <= 0 {
		return 0
	}
	return int(line - 1)
}
