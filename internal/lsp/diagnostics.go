package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/owenrumney/go-lsp/lsp"

	"github.com/infracost/lsp/internal/scanner"
)

// publishDiagnostics sends FinOps and tag policy violations as LSP diagnostics
// for all affected files. It also clears diagnostics for files that no longer
// have violations.
func (s *Server) publishDiagnostics() {
	if s.client == nil {
		return
	}

	s.mu.RLock()
	enabled := s.settings.EnableDiagnostics
	s.mu.RUnlock()
	if enabled != nil && !*enabled {
		s.clearAllDiagnostics()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result := s.getMergedResult()

	byFile := make(map[string][]lsp.Diagnostic)
	addDiag := func(filename string, d []lsp.Diagnostic) {
		if filename == "" {
			return
		}
		absPath, err := filepath.Abs(filename)
		if err != nil {
			return
		}
		if !isLocalFile(absPath) {
			return
		}
		uri := pathToURI(absPath)
		byFile[uri] = append(byFile[uri], d...)
	}

	for _, v := range result.Violations {
		absPath, _ := filepath.Abs(v.Filename)
		if s.ignores.IsIgnored(absPath, v.Address, v.PolicySlug) {
			continue
		}
		addDiag(v.Filename, finopsViolationToDiagnostic(v))
	}
	for _, v := range result.TagViolations {
		diags := tagViolationToDiagnostics(v)
		absPath, _ := filepath.Abs(v.Filename)
		filtered := diags[:0]
		for _, d := range diags {
			slug := tagDiagnosticSlug(v.PolicyName, d.Message)
			if !s.ignores.IsIgnored(absPath, v.Address, slug) {
				filtered = append(filtered, d)
			}
		}
		addDiag(v.Filename, filtered)
	}

	s.mu.Lock()
	prev := s.filesWithDiagnostics
	s.filesWithDiagnostics = make(map[string]struct{}, len(byFile))
	for uri := range byFile {
		s.filesWithDiagnostics[uri] = struct{}{}
	}
	s.mu.Unlock()

	// Clear diagnostics for files that no longer have violations.
	for uri := range prev {
		if _, ok := byFile[uri]; !ok {
			if err := s.client.PublishDiagnostics(ctx, &lsp.PublishDiagnosticsParams{
				URI:         lsp.DocumentURI(uri),
				Diagnostics: []lsp.Diagnostic{},
			}); err != nil {
				slog.Warn("publishDiagnostics: failed to clear", "uri", uri, "error", err)
			}
		}
	}

	// Publish diagnostics grouped by file.
	for uri, diags := range byFile {
		if err := s.client.PublishDiagnostics(ctx, &lsp.PublishDiagnosticsParams{
			URI:         lsp.DocumentURI(uri),
			Diagnostics: diags,
		}); err != nil {
			slog.Warn("publishDiagnostics: failed to publish", "uri", uri, "error", err)
		}
	}

	slog.Debug("publishDiagnostics: done", "files", len(byFile))
}

// finopsViolationToDiagnostic converts a single FinopsViolation to an lsp.Diagnostic.
func finopsViolationToDiagnostic(v scanner.FinopsViolation) []lsp.Diagnostic {
	severity := lsp.SeverityWarning
	if v.BlockPullRequest {
		severity = lsp.SeverityError
	}

	msg := v.Message
	if v.MonthlySavings != nil && !v.MonthlySavings.IsZero() {
		msg = fmt.Sprintf("%s — saves %s/mo", msg, scanner.FormatCost(v.MonthlySavings))
	}
	rng := lsp.Range{
		Start: lsp.Position{Line: safeLineToLSP(v.StartLine), Character: 0},
		End:   lsp.Position{Line: safeLineToLSP(v.EndLine), Character: 0},
	}

	code, _ := json.Marshal(v.PolicySlug)

	return []lsp.Diagnostic{
		{
			Range:    rng,
			Severity: &severity,
			Code:     code,
			Source:   "infracost",
			Message:  msg,
		},
	}
}

// tagViolationToDiagnostics converts a single TagViolation to one or more lsp.Diagnostic.
func tagViolationToDiagnostics(v scanner.TagViolation) []lsp.Diagnostic {
	diags := make([]lsp.Diagnostic, 0, len(v.MissingTags)+len(v.InvalidTags))

	severity := lsp.SeverityWarning
	if v.BlockPR {
		severity = lsp.SeverityError
	}

	rng := lsp.Range{
		Start: lsp.Position{Line: safeLineToLSP(v.StartLine), Character: 0},
		End:   lsp.Position{Line: safeLineToLSP(v.EndLine), Character: 0},
	}

	code, _ := json.Marshal("tag:" + v.PolicyName)

	for _, t := range v.MissingTags {
		msg := fmt.Sprintf("Missing tag: %s", t)
		diags = append(diags, lsp.Diagnostic{
			Range:    rng,
			Severity: &severity,
			Code:     code,
			Source:   "infracost",
			Message:  msg,
		})
	}

	for _, it := range v.InvalidTags {
		msg := fmt.Sprintf("Invalid tag: %s=%s", it.Key, it.Value)
		if it.Suggestion != "" {
			msg += fmt.Sprintf(" (suggested: %s)", it.Suggestion)
		}

		diags = append(diags, lsp.Diagnostic{
			Range:    rng,
			Severity: &severity,
			Code:     code,
			Source:   "infracost",
			Message:  msg,
		})
	}

	return diags
}

// publishAuthDiagnostic sends a single info diagnostic to the given file
// indicating that the user needs to authenticate.
func (s *Server) publishAuthDiagnostic(uri string) {
	if s.client == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s.mu.Lock()
	if s.filesWithDiagnostics == nil {
		s.filesWithDiagnostics = make(map[string]struct{})
	}
	s.filesWithDiagnostics[uri] = struct{}{}
	s.mu.Unlock()

	severity := lsp.SeverityInformation
	if err := s.client.PublishDiagnostics(ctx, &lsp.PublishDiagnosticsParams{
		URI: lsp.DocumentURI(uri),
		Diagnostics: []lsp.Diagnostic{
			{
				Range: lsp.Range{
					Start: lsp.Position{Line: 0, Character: 0},
					End:   lsp.Position{Line: 0, Character: 0},
				},
				Severity: &severity,
				Source:   "infracost",
				Message:  "Infracost: not authenticated. Run `infracost login` to enable cost analysis and FinOps policies.",
			},
		},
	}); err != nil {
		slog.Warn("publishAuthDiagnostic: failed", "uri", uri, "error", err)
	}
}

// clearAllDiagnostics publishes empty diagnostics for every file that
// currently has diagnostics, effectively removing all squiggly lines.
func (s *Server) clearAllDiagnostics() {
	s.mu.Lock()
	prev := s.filesWithDiagnostics
	s.filesWithDiagnostics = make(map[string]struct{})
	s.mu.Unlock()

	if len(prev) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for uri := range prev {
		if err := s.client.PublishDiagnostics(ctx, &lsp.PublishDiagnosticsParams{
			URI:         lsp.DocumentURI(uri),
			Diagnostics: []lsp.Diagnostic{},
		}); err != nil {
			slog.Warn("clearAllDiagnostics: failed", "uri", uri, "error", err)
		}
	}
}

// isLocalFile returns true if the path exists on disk as a regular file.
// This filters out violations from remote modules (e.g. github.com/...) whose
// filenames resolve to non-existent paths.
func isLocalFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
