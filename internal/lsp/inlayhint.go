package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/owenrumney/go-lsp/lsp"

	"github.com/infracost/lsp/internal/scanner"
)

func marshalLabel(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// InlayHint implements server.InlayHintHandler.
// Skipped when the client supports CodeLens to avoid duplicate information.
func (s *Server) InlayHint(_ context.Context, params *lsp.InlayHintParams) ([]lsp.InlayHint, error) {
	if s.clientSupportsCodeLens {
		return nil, nil
	}

	uri := string(params.TextDocument.URI)

	result := s.getMergedResult()
	if len(result.Resources) == 0 && len(result.ModuleCosts) == 0 && len(result.Violations) == 0 && len(result.TagViolations) == 0 {
		slog.Debug("inlayHint: no results cached", "uri", uri)
		return nil, nil
	}

	reqPath := uriToPath(uri)
	scanning := s.isScanning()

	violationsByAddr := make(map[string][]scanner.FinopsViolation)
	for _, v := range result.Violations {
		violationsByAddr[v.Address] = append(violationsByAddr[v.Address], v)
	}
	tagViolationsByAddr := make(map[string][]scanner.TagViolation)
	for _, v := range result.TagViolations {
		tagViolationsByAddr[v.Address] = append(tagViolationsByAddr[v.Address], v)
	}

	hints := make([]lsp.InlayHint, 0, len(result.Resources)+len(result.ModuleCosts))
	for _, r := range result.Resources {
		if r.Filename == "" || r.StartLine == 0 {
			continue
		}

		resPath, err := filepath.Abs(r.Filename)
		if err != nil || resPath != reqPath {
			continue
		}

		line := safeLineToLSP(r.StartLine)
		pos := lsp.Position{Line: line, Character: 999}

		// Cost hint.
		var label string
		switch {
		case scanning:
			label = "Calculating..."
		case !r.IsSupported:
			label = "Not supported"
		default:
			label = scanner.FormatCost(r.MonthlyCost) + "/mo"
		}

		hint := lsp.InlayHint{
			Position:    pos,
			Label:       marshalLabel(label),
			PaddingLeft: ptrTo(true),
		}
		if !scanning && r.IsSupported {
			hint.Tooltip = &lsp.MarkupContent{
				Kind:  lsp.Markdown,
				Value: buildFullHoverMarkdown(r, violationsByAddr[r.Name], tagViolationsByAddr[r.Name]),
			}
		}
		hints = append(hints, hint)

		// FinOps violations hint.
		if vs := violationsByAddr[r.Name]; len(vs) > 0 {
			hints = append(hints, lsp.InlayHint{
				Position:    pos,
				Label:       marshalLabel(fmt.Sprintf("%d FinOps %s", len(vs), pluralize(len(vs), "issue", "issues"))),
				PaddingLeft: ptrTo(true),
			})
		}

		// Tag violations hint.
		if vs := tagViolationsByAddr[r.Name]; len(vs) > 0 {
			hints = append(hints, lsp.InlayHint{
				Position:    pos,
				Label:       marshalLabel(fmt.Sprintf("%d tag %s", len(vs), pluralize(len(vs), "issue", "issues"))),
				PaddingLeft: ptrTo(true),
			})
		}
	}

	for _, mc := range result.ModuleCosts {
		if mc.Filename == "" || mc.StartLine == 0 {
			continue
		}

		mcPath, err := filepath.Abs(mc.Filename)
		if err != nil || mcPath != reqPath {
			continue
		}

		line := safeLineToLSP(mc.StartLine)
		pos := lsp.Position{Line: line, Character: 999}

		var label string
		if scanning {
			label = "Calculating..."
		} else {
			label = fmt.Sprintf("%s/mo (%d %s)", scanner.FormatCost(mc.MonthlyCost), mc.ResourceCount, pluralize(mc.ResourceCount, "resource", "resources"))
		}

		var modResources []scanner.ResourceResult
		for _, r := range result.Resources {
			if strings.HasPrefix(r.Name, mc.Name+".") {
				modResources = append(modResources, r)
			}
		}

		hint := lsp.InlayHint{
			Position:    pos,
			Label:       marshalLabel(label),
			PaddingLeft: ptrTo(true),
		}
		if !scanning {
			hint.Tooltip = &lsp.MarkupContent{
				Kind:  lsp.Markdown,
				Value: buildModuleHoverMarkdown(mc, modResources),
			}
		}
		hints = append(hints, hint)
	}

	slog.Debug("inlayHint: returning", "uri", uri, "hints", len(hints))
	return hints, nil
}
