package lsp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	repoconfig "github.com/infracost/config"
	"github.com/owenrumney/go-lsp/lsp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infracost/lsp/internal/scanner"
)

const scanDebounce = 300 * time.Millisecond

// interactiveScanTimeout bounds a save-triggered scan when the module cache is
// warm. Shorter than the cold-cache budget on purpose: the project's scanning
// flag suppresses rescans for the whole window, so a long budget means a long
// stretch of saves with no feedback.
const interactiveScanTimeout = 2 * time.Minute

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
	// provider plugin conflicts and timeouts. Mark it dirty instead so a
	// follow-up scan runs once the current one finishes.
	if s.markProjectDirtyIfScanning(projectName, uri) {
		slog.Debug("scheduleAnalyze: project already scanning, marking dirty", "project", projectName, "uri", uri)
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
	scanVersion := s.currentScanVersion()
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
	defer func() {
		s.setScanningProject(project.Name, false)
		if dirtyURI, ok := s.popDirtyProject(project.Name); ok {
			slog.Debug("analyze: project was dirtied during scan, re-scanning", "project", project.Name)
			s.scheduleAnalyze(dirtyURI)
		}
	}()
	s.refreshCodeLenses()
	s.refreshInlayHints()

	// A save-triggered scan holds the project's scanning flag for its whole
	// budget, and scheduleAnalyze swallows every save in that window into
	// dirtyProjects. So it only takes the full cold-cache budget when the cache
	// is actually cold; with a warm cache there are no module downloads to wait
	// for, and interactiveScanTimeout keeps the feedback loop short.
	cacheCold := scanner.ModuleCacheIsCold()
	budget := interactiveScanTimeout
	if cacheCold {
		budget = s.scanner.ScanTimeoutOrDefault()
	}

	// The budget lives on a child so the parent still distinguishes "superseded
	// by a newer scan" from "ran out of time".
	scanCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	progress := newProgressReporter(s.client)
	progress.Begin(ctx, scanTitle(project.Name, cacheCold))
	// Every exit path has to tell the client the scan is over, or the webview
	// stays in its scanning state for the rest of the session (FIX-619). The one
	// exception is a superseded scan: its replacement is still running, and
	// scanComplete latches the webview's completed state, which would render
	// this scan's stale results as final.
	defer func() {
		if !s.scanSuperseded(ctx, scanVersion) {
			s.sendScanComplete()
		}
	}()

	start := time.Now()
	result, err := s.scanner.ScanProject(scanCtx, s.workspaceRoot, cfg, project)
	elapsed := time.Since(start)

	if err != nil {
		// A superseded scan is not a failure: whatever replaced it reports for
		// itself.
		if s.scanSuperseded(ctx, scanVersion) {
			slog.Info("analyze: scan cancelled or superseded", "project", project.Name, "elapsed", elapsed)
			endProgress(ctx, progress, "Scan cancelled")
			s.refreshCodeLenses()
			s.refreshInlayHints()
			return
		}

		nctx, ncancel := notifyContext(ctx)
		defer ncancel()

		slog.Error("analyze: scan failed", "project", project.Name, "error", err, "elapsed", elapsed)
		s.setProjectError(project.Name, err)
		message := scanFailureMessage(project.Name, err)
		s.showMessage(nctx, lsp.MessageTypeWarning, message)
		// The same wording as the toast: the spinner must not be the one place
		// the raw "rpc error: code = DeadlineExceeded" still leaks out.
		endProgress(ctx, progress, message)
		s.refreshCodeLenses()
		s.refreshInlayHints()
		return
	}
	s.clearProjectError(project.Name)

	if scanCtx.Err() != nil {
		slog.Info("analyze: scan cancelled after completion", "project", project.Name)
		endProgress(ctx, progress, "Scan cancelled")
		return
	}
	if !s.isCurrentScanVersion(scanVersion) {
		slog.Info("analyze: stale scan, discarding", "project", project.Name)
		endProgress(ctx, progress, "Scan superseded")
		return
	}

	slog.Info("analyze: scan complete",
		"project", project.Name,
		"resources", len(result.Resources),
		"violations", len(result.Violations),
		"errors", len(result.Errors),
		"elapsed", elapsed,
	)

	s.trackRun(scanCtx, result, elapsed)

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

	s.trackDiff(scanCtx, project.Name, result)
	s.setProjectResult(project.Name, result)
	s.refreshCodeLenses()
	s.refreshInlayHints()
	s.publishDiagnostics()

	// Not scanCtx: a scan that finishes just inside its deadline would otherwise
	// have its own "end" notification dropped, leaving the spinner up forever.
	endProgress(ctx, progress, fmt.Sprintf("%d resources, %d violations", len(result.Resources), len(result.Violations)))
}

// endProgress closes the progress token on a context derived for notifications,
// so a scan that ran out of time — or was cancelled — can still retract its own
// spinner. The single way to end progress; the scan context must never carry a
// terminal notification.
func endProgress(ctx context.Context, progress *progressReporter, message string) {
	nctx, cancel := notifyContext(ctx)
	defer cancel()
	progress.End(nctx, message)
}

// workspaceScanBudget is the aggregate ceiling for a whole-workspace scan. Each
// project is allowed its own full budget plus slack for the work outside it, so
// overhead accumulated by earlier projects cannot truncate a later project's
// deadline. Saturating, so a large configured budget cannot overflow into a
// context that is already expired.
func (s *Server) workspaceScanBudget(projects int) time.Duration {
	if projects < 1 {
		projects = 1
	}
	per := s.scanner.ScanTimeoutOrDefault() + workspaceScanSlack
	if projects > int(math.MaxInt64/per) {
		return math.MaxInt64
	}
	return time.Duration(projects) * per
}

// errWorkspaceBudgetExhausted marks projects the scan loop never reached. It
// wraps context.DeadlineExceeded so isDeadlineExceeded classifies it as a
// timeout, which is what it is.
var errWorkspaceBudgetExhausted = fmt.Errorf(
	"workspace scan budget exhausted before this project was scanned: %w", context.DeadlineExceeded)

// recordAbandonedProjects marks the projects a scan gave up on, so a workspace
// that ran out of budget does not render them as "0 resources" — the exact
// ambiguity FIX-619 exists to remove.
func (s *Server) recordAbandonedProjects(projects []*repoconfig.Project) []ProjectScanError {
	out := make([]ProjectScanError, 0, len(projects))
	for _, project := range projects {
		out = append(out, s.setProjectError(project.Name, errWorkspaceBudgetExhausted))
	}
	return out
}

// scanProjectWithBudget scans one project under its own deadline, so one slow
// project cannot consume the budget of the projects after it.
func (s *Server) scanProjectWithBudget(ctx context.Context, dir string, cfg *repoconfig.Config, project *repoconfig.Project) (*scanner.ScanResult, error) {
	scanCtx, cancel := context.WithTimeout(ctx, s.scanner.ScanTimeoutOrDefault())
	defer cancel()
	return s.scanner.ScanProject(scanCtx, dir, cfg, project)
}

// scanTitle names the in-progress work, warning on a cold module cache that the
// scan pays for downloads. cacheCold is passed in rather than sampled here: the
// first project to run populates the shared cache directory, so sampling per
// project would flag only project one while the rest still download.
func scanTitle(projectName string, cacheCold bool) string {
	if cacheCold {
		return fmt.Sprintf("Scanning %s (warming module cache)...", projectName)
	}
	return fmt.Sprintf("Scanning %s...", projectName)
}

// isDeadlineExceeded reports whether err is a timeout, either ours or the
// parser plugin's gRPC deadline.
func isDeadlineExceeded(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded
}

func scanFailureMessage(projectName string, err error) string {
	if isDeadlineExceeded(err) {
		return fmt.Sprintf("Infracost: scanning %s timed out. Remote modules may still have been downloading — try again once the module cache is warm.", projectName)
	}
	return fmt.Sprintf("Infracost: failed to scan %s: %s", projectName, err)
}

func scanFailureSummary(failed []ProjectScanError, total int) string {
	names := make([]string, 0, len(failed))
	timedOut := false
	for _, f := range failed {
		names = append(names, f.Project)
		if f.TimedOut {
			timedOut = true
		}
	}

	msg := fmt.Sprintf("Infracost: failed to scan %d of %d project(s): %s.",
		len(failed), total, strings.Join(names, ", "))
	if timedOut {
		msg += " Scanning timed out — remote modules may still have been downloading, so try again once the module cache is warm."
	}
	return msg + " See the Infracost output for details."
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

	// Cancel-only parent: it carries the progress notifications, which must
	// still be deliverable after the scan work below has run out of time.
	ctx, cancel, scanVersion, ok := s.tryBeginWorkspaceScan()
	if !ok {
		slog.Info("analyzeFullScan: a workspace scan is already running, skipping fallback", "uri", uri)
		return
	}
	defer s.endWorkspaceScan(scanVersion, cancel)

	progress := newProgressReporter(s.client)
	progress.Begin(ctx, "Scanning workspace")
	// LIFO: the spinner ends, then the client refreshes. Skipped when this scan
	// has been superseded: its replacement is still running, and scanComplete
	// latches the webview's completed state.
	defer func() {
		if !s.scanSuperseded(ctx, scanVersion) {
			s.sendScanComplete()
		}
	}()
	defer endProgress(ctx, progress, "Scan complete")

	cfg, err := scanner.LoadConfig(dir, "")
	if err != nil {
		slog.Error("analyzeFullScan: failed to load config", "error", err)
		endProgress(ctx, progress, "Failed to load config")
		return
	}
	if !s.isCurrentScanVersion(scanVersion) {
		slog.Info("analyzeFullScan: stale scan after config load, discarding")
		return
	}
	s.setConfig(cfg)

	scanCtx, scanCancel := context.WithTimeout(ctx, s.workspaceScanBudget(len(cfg.Projects)))
	defer scanCancel()

	// Sampled once: the first project to run populates the shared cache
	// directory, so a per-project check would only ever flag project one.
	cacheCold := scanner.ModuleCacheIsCold()

	totalResources := 0
	totalViolations := 0
	var failed []ProjectScanError

	for i, project := range cfg.Projects {
		if !s.isCurrentScanVersion(scanVersion) {
			slog.Info("analyzeFullScan: stale scan before project, discarding")
			return
		}
		if ctxErr := scanCtx.Err(); ctxErr != nil {
			// Only a deadline means the projects behind this one were abandoned.
			// A cancellation means a replacement scan is already running and
			// reports for itself; calling these projects timed out would be a
			// lie, and would write fake failures into projectErrors.
			if !errors.Is(ctxErr, context.DeadlineExceeded) {
				slog.Info("analyzeFullScan: scan cancelled, discarding",
					"scanned", i, "total", len(cfg.Projects))
				return
			}
			slog.Error("analyzeFullScan: workspace scan budget exhausted, abandoning remaining projects",
				"scanned", i, "total", len(cfg.Projects))
			failed = append(failed, s.recordAbandonedProjects(cfg.Projects[i:])...)
			break
		}
		pct := (i * 100) / len(cfg.Projects)
		progress.Report(ctx, scanTitle(project.Name, cacheCold), pct)

		s.setScanningProject(project.Name, true)

		start := time.Now()
		result, err := s.scanProjectWithBudget(scanCtx, dir, cfg, project)
		elapsed := time.Since(start)

		s.setScanningProject(project.Name, false)

		if err != nil {
			// Checked before recording: a scan killed mid-project fails with a
			// transport error, and storing that would leave a bogus failure in
			// the status the webview reads.
			if !s.isCurrentScanVersion(scanVersion) {
				slog.Info("analyzeFullScan: stale project scan failed, discarding", "name", project.Name, "error", err)
				return
			}
			slog.Error("analyzeFullScan: project scan failed", "name", project.Name, "error", err, "elapsed", elapsed)
			failed = append(failed, s.setProjectError(project.Name, err))
			continue
		}
		s.clearProjectError(project.Name)
		if !s.isCurrentScanVersion(scanVersion) {
			slog.Info("analyzeFullScan: stale project scan, discarding", "name", project.Name)
			return
		}

		slog.Info("analyzeFullScan: project scanned",
			"name", project.Name,
			"resources", len(result.Resources),
			"violations", len(result.Violations),
			"elapsed", elapsed,
		)

		totalResources += len(result.Resources)
		totalViolations += len(result.Violations)

		s.trackDiff(scanCtx, project.Name, result)
		s.setProjectResult(project.Name, result)
		s.refreshCodeLenses()
		s.refreshInlayHints()
		s.publishDiagnostics()
	}

	// A silent "0 resources" is ambiguous — it can mean "nothing to cost" or
	// "we failed to scan". Surface failures so the user can tell them apart.
	if len(failed) > 0 {
		nctx, ncancel := notifyContext(ctx)
		defer ncancel()
		s.showMessage(nctx, lsp.MessageTypeWarning, scanFailureSummary(failed, len(cfg.Projects)))
	}

	endProgress(ctx, progress, fmt.Sprintf("Scan complete — %d resources, %d violations", totalResources, totalViolations))
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
