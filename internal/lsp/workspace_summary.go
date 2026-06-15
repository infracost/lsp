package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/infracost/lsp/internal/scanner"
)

// remoteModulesGroup is the synthetic top-level tree node under which resources
// from remote/registry modules are grouped, instead of shredding their source
// URL into one node per path segment.
const remoteModulesGroup = "Remote modules"

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
	currency := s.currency()

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
		uri       string
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

		var relPath, uri string
		if scanner.IsRemoteSource(r.Filename) {
			// Group under a single "Remote modules" node with a clean
			// owner/repo@ref/file label rather than the raw source URL.
			relPath = remoteModulePath(r.Filename)
			uri = r.Filename
		} else {
			absPath, err := filepath.Abs(r.Filename)
			if err != nil {
				continue
			}
			relPath, err = filepath.Rel(root, absPath)
			if err != nil || strings.HasPrefix(relPath, "..") {
				relPath = absPath
			}
			// Normalise to forward slashes for consistent JS path splitting.
			relPath = filepath.ToSlash(relPath)
			uri = pathToURI(absPath)
		}

		entry := byFile[relPath]
		if entry == nil {
			entry = &fileEntry{relPath: relPath, uri: uri}
			byFile[relPath] = entry
		}
		res := WorkspaceSummaryResource{
			Name:         r.Name,
			Line:         max(0, int(r.StartLine)-1),
			MonthlyCost:  scanner.FormatCostCurrency(r.MonthlyCost, currency),
			PolicyIssues: policyIssues,
			TagIssues:    tagIssues,
		}
		// The merged result can list the same resource twice; don't duplicate
		// the tree row.
		if !containsResource(entry.resources, res) {
			entry.resources = append(entry.resources, res)
		}
	}

	files := make([]WorkspaceSummaryFile, 0, len(byFile))
	for _, entry := range byFile {
		files = append(files, WorkspaceSummaryFile{
			Path:      entry.relPath,
			URI:       entry.uri,
			Resources: entry.resources,
		})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})

	return WorkspaceSummaryResult{Files: files}, nil
}

// remoteModulePath turns a remote module source URL into a clean, slash-
// delimited tree path under the "Remote modules" node, e.g.
//
//	https://github.com/RaJiska/terraform-aws-fck-nat/blob/1.4.0/ec2.tf#L58-L118
//	-> Remote modules/RaJiska/terraform-aws-fck-nat@1.4.0/ec2.tf
//
// The extension splits the path on "/" to build the tree, so each segment
// becomes a node. Unrecognised URL shapes fall back to grouping the resource
// directly under the "Remote modules" node.
func remoteModulePath(rawURL string) string {
	trimmed := strings.TrimPrefix(rawURL, "git::")
	u, err := url.Parse(trimmed)
	if err != nil || u.Path == "" {
		return remoteModulesGroup
	}

	parts := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	if len(parts) == 0 {
		return remoteModulesGroup
	}

	var owner, repo, ref string
	if len(parts) >= 2 {
		owner, repo = parts[0], parts[1]
	}
	// GitHub/GitLab/Bitbucket layouts encode the ref after blob/tree/raw/src.
	for i, p := range parts {
		if (p == "blob" || p == "tree" || p == "raw" || p == "src") && i+1 < len(parts) {
			ref = parts[i+1]
			break
		}
	}
	// Terraform git sources encode the ref in a ?ref= query instead.
	if ref == "" {
		ref = u.Query().Get("ref")
	}

	label := repo
	if owner != "" && repo != "" {
		label = owner + "/" + repo
	}
	if label != "" && ref != "" {
		label += "@" + ref
	}

	segs := []string{remoteModulesGroup}
	if label != "" {
		segs = append(segs, label)
	} else {
		// No recognisable owner/repo; group under the first path segment.
		segs = append(segs, parts[0])
	}
	// The trailing segment is the source file, but only when the URL points
	// deeper than owner/repo (otherwise it just repeats the repo segment).
	if len(parts) > 2 {
		segs = append(segs, parts[len(parts)-1])
	}
	return strings.Join(segs, "/")
}

// containsResource reports whether rs already holds a resource with the same
// name and line, so the merged result's duplicates aren't shown twice.
func containsResource(rs []WorkspaceSummaryResource, r WorkspaceSummaryResource) bool {
	for _, x := range rs {
		if x.Name == r.Name && x.Line == r.Line {
			return true
		}
	}
	return false
}
