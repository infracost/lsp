package lsp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/infracost/lsp/internal/scanner"
	"github.com/infracost/lsp/version"
)

// Response type for infracost/status.

// GuardrailStatus is a summary of a triggered guardrail for the webview banner.
type GuardrailStatus struct {
	Name             string `json:"name"`
	Message          string `json:"message"`
	BlockPR          bool   `json:"blockPr"`
	TotalMonthlyCost string `json:"totalMonthlyCost,omitempty"`
	Threshold        string `json:"threshold,omitempty"`
}

type StatusResult struct {
	Version             string            `json:"version"`
	WorkspaceRoot       string            `json:"workspaceRoot"`
	LoggedIn            bool              `json:"loggedIn"`
	Scanning            bool              `json:"scanning"`
	ProjectCount        int               `json:"projectCount"`
	ProjectNames        []string          `json:"projectNames"`
	ResourceCount       int               `json:"resourceCount"`
	ViolationCount      int               `json:"violationCount"`
	TagIssueCount       int               `json:"tagIssueCount"`
	ConfigFound         bool              `json:"configFound"`
	TriggeredGuardrails []GuardrailStatus `json:"triggeredGuardrails"`
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
	for _, g := range merged.GuardrailResults {
		if !g.Triggered {
			continue
		}
		gs := GuardrailStatus{
			Name:    g.GuardrailName,
			Message: g.Message,
			BlockPR: g.BlockPR,
		}
		if g.TotalMonthlyCost != nil && !g.TotalMonthlyCost.IsZero() {
			gs.TotalMonthlyCost = scanner.FormatCost(g.TotalMonthlyCost)
		}
		switch {
		case g.TotalThreshold != nil && !g.TotalThreshold.IsZero():
			gs.Threshold = scanner.FormatCost(g.TotalThreshold) + "/mo"
		case g.IncreaseThreshold != nil && !g.IncreaseThreshold.IsZero() && g.IncreasePercentThreshold != nil && !g.IncreasePercentThreshold.IsZero():
			f := g.IncreasePercentThreshold.Float64()
			gs.Threshold = fmt.Sprintf("%s/mo / %.1f%%", scanner.FormatCost(g.IncreaseThreshold), f)
		case g.IncreaseThreshold != nil && !g.IncreaseThreshold.IsZero():
			gs.Threshold = scanner.FormatCost(g.IncreaseThreshold) + "/mo"
		case g.IncreasePercentThreshold != nil && !g.IncreasePercentThreshold.IsZero():
			f := g.IncreasePercentThreshold.Float64()
			gs.Threshold = fmt.Sprintf("%.1f%%", f)
		}
		result.TriggeredGuardrails = append(result.TriggeredGuardrails, gs)
	}

	return result, nil
}
