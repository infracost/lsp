package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"

	"github.com/owenrumney/go-lsp/lsp"

	"github.com/infracost/go-proto/pkg/rat"
	"github.com/infracost/lsp/internal/scanner"
)

type codeLensEntry struct {
	lens     lsp.CodeLens
	kind     int
	amount   *rat.Rat
	priority int
}

// CodeLens implements server.CodeLensHandler.
func (s *Server) CodeLens(_ context.Context, params *lsp.CodeLensParams) ([]lsp.CodeLens, error) {
	uri := string(params.TextDocument.URI)

	result := s.getMergedResult()
	if len(result.Resources) == 0 && len(result.ModuleCosts) == 0 && len(result.Violations) == 0 && len(result.TagViolations) == 0 {
		slog.Debug("codeLens: no results cached", "uri", uri)
		return nil, nil
	}

	reqPath := filepath.Clean(uriToPath(uri))
	currency := s.currency()

	// Build maps of resource address → violations.
	violationsByAddr := make(map[string][]scanner.FinopsViolation)
	for _, v := range result.Violations {
		violationsByAddr[v.Address] = append(violationsByAddr[v.Address], v)
	}
	tagViolationsByAddr := make(map[string][]scanner.TagViolation)
	for _, v := range result.TagViolations {
		tagViolationsByAddr[v.Address] = append(tagViolationsByAddr[v.Address], v)
	}

	entries := make([]codeLensEntry, 0, len(result.Resources)+len(result.ModuleCosts))
	seen := make(map[string]bool)
	costByLine := make(map[int]codeLensEntry)

	addIssueLens := func(rng lsp.Range, cmd *lsp.Command) {
		key := codeLensDedupeKey(rng, cmd)
		if seen[key] {
			return
		}
		seen[key] = true
		entries = append(entries, codeLensEntry{lens: lsp.CodeLens{Range: rng, Command: cmd}, kind: 1})
	}
	addCostLens := func(rng lsp.Range, cmd *lsp.Command, amount *rat.Rat, priority int) {
		line := rng.Start.Line
		candidate := codeLensEntry{lens: lsp.CodeLens{Range: rng, Command: cmd}, kind: 0, amount: amount, priority: priority}
		if existing, ok := costByLine[line]; ok && betterCodeLensCost(existing, candidate) {
			return
		}
		costByLine[line] = candidate
	}

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

		// Cost lens — omitted when no pricing data is available. Only one cost lens
		// is shown per line; picks the highest cost, with module aggregate costs winning ties.
		switch {
		case !r.IsSupported:
			addCostLens(rng, revealResourceCommand("Not supported", uri, line, r.Name), nil, 1)
		case r.IsFree:
			addCostLens(rng, revealResourceCommand(scanner.FormatCostCurrency(r.MonthlyCost, currency)+"/mo", uri, line, r.Name), rat.Zero, 1)
		case r.MonthlyCost != nil && !r.MonthlyCost.IsZero():
			addCostLens(rng, revealResourceCommand(scanner.FormatCostCurrency(r.MonthlyCost, currency)+"/mo", uri, line, r.Name), r.MonthlyCost, 1)
		}

		// FinOps violations lens.
		if vs := violationsByAddr[r.Name]; len(vs) > 0 {
			label := fmt.Sprintf("%d FinOps %s", len(vs), pluralize(len(vs), "issue", "issues"))
			addIssueLens(rng, revealResourceCommand(label, uri, line, r.Name))
		}

		// Tag violations lens.
		if vs := tagViolationsByAddr[r.Name]; len(vs) > 0 {
			label := fmt.Sprintf("%d tag %s", len(vs), pluralize(len(vs), "issue", "issues"))
			addIssueLens(rng, revealResourceCommand(label, uri, line, r.Name))
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

		title := fmt.Sprintf("%s/mo (%d %s)", scanner.FormatCostCurrency(mc.MonthlyCost, currency), mc.ResourceCount, pluralize(mc.ResourceCount, "resource", "resources"))
		addCostLens(rng, revealResourceCommand(title, uri, line), mc.MonthlyCost, 1000+mc.ResourceCount)
	}

	for _, entry := range costByLine {
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		li := entries[i].lens.Range.Start.Line
		lj := entries[j].lens.Range.Start.Line
		if li != lj {
			return li < lj
		}
		if entries[i].kind != entries[j].kind {
			return entries[i].kind < entries[j].kind
		}
		return codeLensTitle(entries[i].lens) < codeLensTitle(entries[j].lens)
	})

	lenses := make([]lsp.CodeLens, 0, len(entries))
	for _, entry := range entries {
		lenses = append(lenses, entry.lens)
	}

	slog.Debug("codeLens: returning", "uri", uri, "lenses", len(lenses))
	return lenses, nil
}

func betterCodeLensCost(existing, candidate codeLensEntry) bool {
	if existing.amount != nil && candidate.amount != nil {
		if existing.amount.GreaterThan(candidate.amount) {
			return true
		}
		if candidate.amount.GreaterThan(existing.amount) {
			return false
		}
		return existing.priority >= candidate.priority
	}
	if existing.amount != nil {
		return true
	}
	if candidate.amount != nil {
		return false
	}
	return existing.priority >= candidate.priority
}

func codeLensTitle(lens lsp.CodeLens) string {
	if lens.Command == nil {
		return ""
	}
	return lens.Command.Title
}

func codeLensDedupeKey(rng lsp.Range, cmd *lsp.Command) string {
	if cmd == nil {
		return fmt.Sprintf("%d:%d:<nil>", rng.Start.Line, rng.Start.Character)
	}
	return fmt.Sprintf("%d:%d:%s:%s", rng.Start.Line, rng.Start.Character, cmd.Command, cmd.Title)
}

func revealResourceCommand(title, uri string, line int, address ...string) *lsp.Command {
	// json.Marshal cannot fail for string or int values.
	uriArg, _ := json.Marshal(uri)   //nolint:errcheck
	lineArg, _ := json.Marshal(line) //nolint:errcheck
	args := []json.RawMessage{uriArg, lineArg}
	if len(address) > 0 && address[0] != "" {
		addrArg, _ := json.Marshal(address[0]) //nolint:errcheck
		args = append(args, addrArg)
	}
	return &lsp.Command{
		Title:     title,
		Command:   "infracost.revealResource",
		Arguments: args,
	}
}

func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
