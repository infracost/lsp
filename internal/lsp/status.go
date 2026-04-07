package lsp

import (
	"context"
	"encoding/json"

	"github.com/infracost/lsp/version"
)

// Response type for infracost/status.

type StatusResult struct {
	Version        string   `json:"version"`
	WorkspaceRoot  string   `json:"workspaceRoot"`
	LoggedIn       bool     `json:"loggedIn"`
	Scanning       bool     `json:"scanning"`
	ProjectCount   int      `json:"projectCount"`
	ProjectNames   []string `json:"projectNames"`
	ResourceCount  int      `json:"resourceCount"`
	ViolationCount int      `json:"violationCount"`
	TagIssueCount  int      `json:"tagIssueCount"`
	ConfigFound    bool     `json:"configFound"`
}

// HandleStatus handles the infracost/status custom request.
func (s *Server) HandleStatus(_ context.Context, _ json.RawMessage) (any, error) {
	result := StatusResult{
		Version:       version.Version,
		WorkspaceRoot: s.workspaceRoot,
		LoggedIn:      s.scanner != nil && s.scanner.HasTokenSource(),
		Scanning:      s.isScanning(),
	}

	cfg := s.getConfig()
	if cfg != nil {
		result.ConfigFound = true
		result.ProjectCount = len(cfg.Projects)
		names := make([]string, 0, len(cfg.Projects))
		for _, p := range cfg.Projects {
			names = append(names, p.Name)
		}
		result.ProjectNames = names
	}

	merged := s.getMergedResult()
	result.ResourceCount = len(merged.Resources)
	result.ViolationCount = len(merged.Violations)
	for _, v := range merged.TagViolations {
		result.TagIssueCount += len(v.MissingTags) + len(v.InvalidTags)
	}

	return result, nil
}
