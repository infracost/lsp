package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/owenrumney/go-lsp/lsp"

	"github.com/infracost/lsp/internal/scanner"
)

// CodeLens implements server.CodeLensHandler.
func (s *Server) CodeLens(_ context.Context, params *lsp.CodeLensParams) ([]lsp.CodeLens, error) {
	uri := string(params.TextDocument.URI)

	result := s.getMergedResult()
	if len(result.Resources) == 0 && len(result.ModuleCosts) == 0 && len(result.Violations) == 0 && len(result.TagViolations) == 0 {
		slog.Debug("codeLens: no results cached", "uri", uri)
		return nil, nil
	}

	reqPath := filepath.Clean(uriToPath(uri))

	// Build maps of resource address → violations.
	violationsByAddr := make(map[string][]scanner.FinopsViolation)
	for _, v := range result.Violations {
		violationsByAddr[v.Address] = append(violationsByAddr[v.Address], v)
	}
	tagViolationsByAddr := make(map[string][]scanner.TagViolation)
	for _, v := range result.TagViolations {
		tagViolationsByAddr[v.Address] = append(tagViolationsByAddr[v.Address], v)
	}

	lenses := make([]lsp.CodeLens, 0, len(result.Resources)+len(result.ModuleCosts))
	for _, r := range result.Resources {
		if r.Filename == "" || r.StartLine == 0 {
			continue
		}

		resPath, err := filepath.Abs(r.Filename)
		if err != nil || resPath != reqPath {
			continue
		}

		line := safeLineToLSP(r.StartLine)
		rng := lsp.Range{
			Start: lsp.Position{Line: line, Character: 0},
			End:   lsp.Position{Line: line, Character: 0},
		}

		// Cost lens.
		var title string
		switch {
		case !r.IsSupported:
			title = "Not supported"
		default:
			title = scanner.FormatCost(r.MonthlyCost) + "/mo"
		}
		lenses = append(lenses, lsp.CodeLens{
			Range:   rng,
			Command: revealResourceCommand(title, uri, line),
		})

		// FinOps violations lens.
		if vs := violationsByAddr[r.Name]; len(vs) > 0 {
			label := fmt.Sprintf("%d FinOps %s", len(vs), pluralize(len(vs), "issue", "issues"))
			lenses = append(lenses, lsp.CodeLens{
				Range:   rng,
				Command: revealResourceCommand(label, uri, line),
			})
		}

		// Tag violations lens.
		if vs := tagViolationsByAddr[r.Name]; len(vs) > 0 {
			label := fmt.Sprintf("%d tag %s", len(vs), pluralize(len(vs), "issue", "issues"))
			lenses = append(lenses, lsp.CodeLens{
				Range:   rng,
				Command: revealResourceCommand(label, uri, line),
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
		rng := lsp.Range{
			Start: lsp.Position{Line: line, Character: 0},
			End:   lsp.Position{Line: line, Character: 0},
		}

		title := fmt.Sprintf("%s/mo (%d %s)", scanner.FormatCost(mc.MonthlyCost), mc.ResourceCount, pluralize(mc.ResourceCount, "resource", "resources"))
		lenses = append(lenses, lsp.CodeLens{
			Range:   rng,
			Command: revealResourceCommand(title, uri, line),
		})
	}

	slog.Debug("codeLens: returning", "uri", uri, "lenses", len(lenses))
	return lenses, nil
}

func revealResourceCommand(title, uri string, line int) *lsp.Command {
	// json.Marshal cannot fail for string or int values.
	uriArg, _ := json.Marshal(uri)   //nolint:errcheck
	lineArg, _ := json.Marshal(line) //nolint:errcheck
	return &lsp.Command{
		Title:     title,
		Command:   "infracost.revealResource",
		Arguments: []json.RawMessage{uriArg, lineArg},
	}
}

func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
