package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/infracost/lsp/internal/scanner"
)

type WorkspaceSummaryResult struct {
	Files []WorkspaceSummaryFile `json:"files"`
}

type WorkspaceSummaryFile struct {
	Path      string                     `json:"path"` // relative to workspace root
	URI       string                     `json:"uri"`
	Resources []WorkspaceSummaryResource `json:"resources"`
}

type WorkspaceSummaryResource struct {
	Name         string `json:"name"`
	Line         int    `json:"line"`
	MonthlyCost  string `json:"monthlyCost"`
	PolicyIssues int    `json:"policyIssues"`
	TagIssues    int    `json:"tagIssues"`
}

// HandleWorkspaceSummary returns all resources with FinOps or tag policy issues,
// grouped by file and with paths relative to the workspace root.
func (s *Server) HandleWorkspaceSummary(_ context.Context, _ json.RawMessage) (any, error) {
	if s.isScanning() {
		return WorkspaceSummaryResult{}, nil
	}

	result := s.getMergedResult()

	s.mu.RLock()
	root := s.workspaceRoot
	s.mu.RUnlock()

	// Count policy violations per resource address.
	policyCounts := make(map[string]int)
	for _, v := range result.Violations {
		absPath, _ := filepath.Abs(v.Filename)
		if !s.ignores.IsIgnored(absPath, v.Address, v.PolicySlug) {
			policyCounts[v.Address]++
		}
	}

	// Count tag violations per resource address.
	tagCounts := make(map[string]int)
	for _, v := range result.TagViolations {
		absPath, _ := filepath.Abs(v.Filename)
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

	// Group resources by file, only including those with issues.
	type fileEntry struct {
		relPath   string
		absPath   string
		resources []WorkspaceSummaryResource
	}
	byFile := make(map[string]*fileEntry)

	for _, r := range result.Resources {
		if r.Filename == "" {
			continue
		}
		policyIssues := policyCounts[r.Name]
		tagIssues := tagCounts[r.Name]
		if policyIssues == 0 && tagIssues == 0 {
			continue
		}

		absPath, err := filepath.Abs(r.Filename)
		if err != nil {
			continue
		}

		relPath, err := filepath.Rel(root, absPath)
		if err != nil || strings.HasPrefix(relPath, "..") {
			relPath = absPath
		}
		// Normalise to forward slashes for consistent JS path splitting.
		relPath = filepath.ToSlash(relPath)

		if byFile[relPath] == nil {
			byFile[relPath] = &fileEntry{relPath: relPath, absPath: absPath}
		}
		byFile[relPath].resources = append(byFile[relPath].resources, WorkspaceSummaryResource{
			Name:         r.Name,
			Line:         max(0, int(r.StartLine)-1),
			MonthlyCost:  scanner.FormatCost(r.MonthlyCost),
			PolicyIssues: policyIssues,
			TagIssues:    tagIssues,
		})
	}

	files := make([]WorkspaceSummaryFile, 0, len(byFile))
	for _, entry := range byFile {
		files = append(files, WorkspaceSummaryFile{
			Path:      entry.relPath,
			URI:       pathToURI(entry.absPath),
			Resources: entry.resources,
		})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})

	return WorkspaceSummaryResult{Files: files}, nil
}
