package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/infracost/lsp/internal/scanner"
)

// Response types for infracost/fileSummary.

type FileSummaryResult struct {
	Resources []FileSummaryResource `json:"resources"`
}

type FileSummaryResource struct {
	Name         string `json:"name"`
	Line         int    `json:"line"`
	MonthlyCost  string `json:"monthlyCost"`
	PolicyIssues int    `json:"policyIssues"`
	TagIssues    int    `json:"tagIssues"`
}

type fileSummaryParams struct {
	URI string `json:"uri"`
}

// HandleFileSummary handles the infracost/fileSummary custom request.
// It returns a summary of all resources in the given file.
func (s *Server) HandleFileSummary(_ context.Context, params json.RawMessage) (any, error) {
	var p fileSummaryParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}

	slog.Debug("fileSummary: request", "uri", p.URI)

	if s.isScanning() {
		return FileSummaryResult{}, nil
	}

	result := s.getMergedResult()
	if len(result.Resources) == 0 {
		return FileSummaryResult{}, nil
	}

	reqPath := filepath.Clean(uriToPath(p.URI))

	// Count policy violations per resource address.
	policyCounts := make(map[string]int)
	for _, v := range result.Violations {
		absPath, _ := filepath.Abs(v.Filename)
		if absPath == reqPath && !s.ignores.IsIgnored(absPath, v.Address, v.PolicySlug) {
			policyCounts[v.Address]++
		}
	}

	// Count tag violations per resource address.
	tagCounts := make(map[string]int)
	for _, v := range result.TagViolations {
		absPath, _ := filepath.Abs(v.Filename)
		if absPath != reqPath {
			continue
		}
		for _, t := range v.MissingTags {
			slug := tagDiagnosticSlug(v.PolicyName, fmt.Sprintf("Missing tag: %s", t))
			if !s.ignores.IsIgnored(absPath, v.Address, slug) {
				tagCounts[v.Address]++
			}
		}
		for _, t := range v.InvalidTags {
			msg := fmt.Sprintf("Invalid tag: %s=%s", t.Key, t.Value)
			if t.Suggestion != "" {
				msg += fmt.Sprintf(" (suggested: %s)", t.Suggestion)
			}
			slug := tagDiagnosticSlug(v.PolicyName, msg)
			if !s.ignores.IsIgnored(absPath, v.Address, slug) {
				tagCounts[v.Address]++
			}
		}
	}

	// Build response with all resources in the file.
	var resources []FileSummaryResource //nolint:prealloc
	for _, r := range result.Resources {
		if r.Filename == "" {
			continue
		}
		resPath, err := filepath.Abs(r.Filename)
		if err != nil || resPath != reqPath {
			continue
		}
		resources = append(resources, FileSummaryResource{
			Name:         r.Name,
			Line:         max(0, int(r.StartLine)-1), // Convert to 0-based for LSP.
			MonthlyCost:  scanner.FormatCost(r.MonthlyCost),
			PolicyIssues: policyCounts[r.Name],
			TagIssues:    tagCounts[r.Name],
		})
	}

	return FileSummaryResult{Resources: resources}, nil
}
