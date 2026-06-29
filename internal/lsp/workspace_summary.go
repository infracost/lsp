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
	Tree  []WorkspaceSummaryNode `json:"tree,omitempty"`
}

type WorkspaceSummaryFile struct {
	Path      string                     `json:"path"` // relative to workspace root
	URI       string                     `json:"uri"`
	Openable  bool                       `json:"openable"`
	Resources []WorkspaceSummaryResource `json:"resources"`
}

type WorkspaceSummaryResource struct {
	Name         string `json:"name"`
	Line         int    `json:"line"`
	MonthlyCost  string `json:"monthlyCost"`
	PolicyIssues int    `json:"policyIssues"`
	TagIssues    int    `json:"tagIssues"`
}

// WorkspaceSummaryNode is a semantic tree for clients that want to show module
// call chains instead of just grouping resources by source file.
type WorkspaceSummaryNode struct {
	Type         string                 `json:"type"`
	Label        string                 `json:"label"`
	Path         string                 `json:"path,omitempty"`
	URI          string                 `json:"uri,omitempty"`
	Openable     bool                   `json:"openable"`
	Line         int                    `json:"line,omitempty"`
	MonthlyCost  string                 `json:"monthlyCost,omitempty"`
	PolicyIssues int                    `json:"policyIssues,omitempty"`
	TagIssues    int                    `json:"tagIssues,omitempty"`
	Children     []WorkspaceSummaryNode `json:"children,omitempty"`
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
	displayRemoteModules := s.settings.DisplayRemoteModulesInTree
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
		openable  bool
		resources []WorkspaceSummaryResource
	}
	byFile := make(map[string]*fileEntry)
	tree := make([]WorkspaceSummaryNode, 0)

	for _, r := range result.Resources {
		if r.Filename == "" {
			continue
		}
		policyIssues := policyCounts[r.Name]
		tagIssues := tagCounts[r.Name]
		if policyIssues == 0 && tagIssues == 0 {
			continue
		}

		relPath, uri, openable, ok := workspaceSummarySource(root, r.Filename, displayRemoteModules)
		if !ok {
			continue
		}

		entry := byFile[relPath]
		if entry == nil {
			entry = &fileEntry{relPath: relPath, uri: uri, openable: openable}
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
		insertWorkspaceSummaryResource(&tree, root, r, res, relPath, uri, openable, displayRemoteModules)
	}

	files := make([]WorkspaceSummaryFile, 0, len(byFile))
	for _, entry := range byFile {
		files = append(files, WorkspaceSummaryFile{
			Path:      entry.relPath,
			URI:       entry.uri,
			Openable:  entry.openable,
			Resources: entry.resources,
		})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	sortWorkspaceSummaryNodes(tree)

	return WorkspaceSummaryResult{Files: files, Tree: tree}, nil
}

func workspaceSummarySource(root, filename string, displayRemoteModules bool) (string, string, bool, bool) {
	if filename == "" {
		return "", "", false, false
	}
	if scanner.IsRemoteSource(filename) {
		if !displayRemoteModules {
			return "", "", false, false
		}
		return remoteModulePath(filename), filename, false, true
	}

	absPath, err := filepath.Abs(filename)
	if err != nil {
		return "", "", false, false
	}
	relPath, err := filepath.Rel(root, absPath)
	if err != nil || strings.HasPrefix(relPath, "..") {
		relPath = absPath
	}
	return filepath.ToSlash(relPath), pathToURI(absPath), true, true
}

func insertWorkspaceSummaryResource(tree *[]WorkspaceSummaryNode, root string, r scanner.ResourceResult, res WorkspaceSummaryResource, resourcePath, resourceURI string, resourceOpenable, displayRemoteModules bool) {
	children := tree
	currentFilePath := ""
	currentModulePath := ""

	for _, call := range r.ModuleCallStack {
		if !strings.HasPrefix(r.Name, call.Name+".") {
			continue
		}
		path, uri, openable, ok := workspaceSummarySource(root, call.Filename, displayRemoteModules)
		if !ok {
			continue
		}
		if path != currentFilePath {
			file := upsertWorkspaceSummaryFileNode(children, WorkspaceSummaryNode{
				Type:     "file",
				Label:    path,
				Path:     path,
				URI:      uri,
				Openable: openable,
			}, currentFilePath == "")
			children = &file.Children
			currentFilePath = path
		}

		if call.Name == currentModulePath {
			continue
		}
		module := upsertWorkspaceSummaryNode(children, WorkspaceSummaryNode{
			Type:     "module",
			Label:    moduleCallLabel(call.Name),
			Path:     call.Name,
			URI:      uri,
			Openable: openable,
			Line:     max(0, int(call.StartLine)-1),
		})
		children = &module.Children
		currentModulePath = call.Name
	}

	if resourcePath != currentFilePath {
		file := upsertWorkspaceSummaryFileNode(children, WorkspaceSummaryNode{
			Type:     "file",
			Label:    resourcePath,
			Path:     resourcePath,
			URI:      resourceURI,
			Openable: resourceOpenable,
		}, currentFilePath == "")
		children = &file.Children
	}

	upsertWorkspaceSummaryNode(children, WorkspaceSummaryNode{
		Type:         "resource",
		Label:        workspaceSummaryResourceLabel(r.Name, r.ModuleCallStack),
		Path:         r.Name,
		URI:          resourceURI,
		Openable:     resourceOpenable,
		Line:         res.Line,
		MonthlyCost:  res.MonthlyCost,
		PolicyIssues: res.PolicyIssues,
		TagIssues:    res.TagIssues,
	})
}

func upsertWorkspaceSummaryFileNode(nodes *[]WorkspaceSummaryNode, node WorkspaceSummaryNode, splitFolders bool) *WorkspaceSummaryNode {
	parts := strings.Split(filepath.ToSlash(node.Path), "/")
	if !splitFolders || len(parts) <= 1 {
		node.Label = parts[len(parts)-1]
		return upsertWorkspaceSummaryNode(nodes, node)
	}

	children := nodes
	for i, part := range parts[:len(parts)-1] {
		folder := upsertWorkspaceSummaryNode(children, WorkspaceSummaryNode{
			Type:     "folder",
			Label:    part,
			Path:     strings.Join(parts[:i+1], "/"),
			Openable: false,
		})
		children = &folder.Children
	}
	node.Label = parts[len(parts)-1]
	return upsertWorkspaceSummaryNode(children, node)
}

func upsertWorkspaceSummaryNode(nodes *[]WorkspaceSummaryNode, node WorkspaceSummaryNode) *WorkspaceSummaryNode {
	for i := range *nodes {
		current := &(*nodes)[i]
		if current.Type == node.Type && current.Path == node.Path && current.URI == node.URI && current.Line == node.Line {
			return current
		}
	}
	*nodes = append(*nodes, node)
	return &(*nodes)[len(*nodes)-1]
}

func sortWorkspaceSummaryNodes(nodes []WorkspaceSummaryNode) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Type != nodes[j].Type {
			return workspaceSummaryNodeOrder(nodes[i].Type) < workspaceSummaryNodeOrder(nodes[j].Type)
		}
		return nodes[i].Label < nodes[j].Label
	})
	for i := range nodes {
		sortWorkspaceSummaryNodes(nodes[i].Children)
	}
}

func workspaceSummaryNodeOrder(t string) int {
	switch t {
	case "file":
		return 0
	case "module":
		return 1
	case "resource":
		return 2
	default:
		return 3
	}
}

func moduleCallLabel(name string) string {
	parts := strings.Split(name, ".")
	if len(parts) >= 2 {
		return "module." + parts[len(parts)-1]
	}
	return name
}

func workspaceSummaryResourceLabel(name string, stack []scanner.ModuleCall) string {
	for i := len(stack) - 1; i >= 0; i-- {
		prefix := stack[i].Name + "."
		if strings.HasPrefix(name, prefix) {
			return strings.TrimPrefix(name, prefix)
		}
	}
	return name
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
