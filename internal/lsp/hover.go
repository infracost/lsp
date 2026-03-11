package lsp

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"github.com/owenrumney/go-lsp/lsp"

	"github.com/infracost/lsp/internal/scanner"
)

// Hover implements server.HoverHandler.
func (s *Server) Hover(_ context.Context, params *lsp.HoverParams) (*lsp.Hover, error) {
	uri := string(params.TextDocument.URI)
	slog.Debug("hover: request", "uri", uri, "line", params.Position.Line, "char", params.Position.Character)

	result := s.getMergedResult()
	if len(result.Resources) == 0 && len(result.ModuleCosts) == 0 {
		return nil, nil
	}

	reqPath := filepath.Clean(uriToPath(uri))
	line := int64(params.Position.Line) + 1 // LSP is 0-based, our data is 1-based

	// Build violations lookup.
	violationsByAddr := make(map[string][]scanner.FinopsViolation)
	for _, v := range result.Violations {
		violationsByAddr[v.Address] = append(violationsByAddr[v.Address], v)
	}
	tagViolationsByAddr := make(map[string][]scanner.TagViolation)
	for _, v := range result.TagViolations {
		tagViolationsByAddr[v.Address] = append(tagViolationsByAddr[v.Address], v)
	}

	for _, r := range result.Resources {
		if r.Filename == "" {
			continue
		}

		resPath, err := filepath.Abs(r.Filename)
		if err != nil || resPath != reqPath {
			continue
		}

		if line < r.StartLine || line > r.EndLine {
			continue
		}

		violations := violationsByAddr[r.Name]
		tagViolations := tagViolationsByAddr[r.Name]

		md := buildFullHoverMarkdown(r, violations, tagViolations)
		return &lsp.Hover{
			Contents: lsp.MarkupContent{
				Kind:  lsp.Markdown,
				Value: md,
			},
			Range: &lsp.Range{
				Start: lsp.Position{Line: safeLineToLSP(r.StartLine), Character: 0},
				End:   lsp.Position{Line: safeLineToLSP(r.EndLine), Character: 0},
			},
		}, nil
	}

	for _, mc := range result.ModuleCosts {
		if mc.Filename == "" {
			continue
		}

		mcPath, err := filepath.Abs(mc.Filename)
		if err != nil || mcPath != reqPath {
			continue
		}

		if line < mc.StartLine || line > mc.EndLine {
			continue
		}

		var modResources []scanner.ResourceResult
		for _, r := range result.Resources {
			if strings.HasPrefix(r.Name, mc.Name+".") {
				modResources = append(modResources, r)
			}
		}

		md := buildModuleHoverMarkdown(mc, modResources)
		return &lsp.Hover{
			Contents: lsp.MarkupContent{
				Kind:  lsp.Markdown,
				Value: md,
			},
			Range: &lsp.Range{
				Start: lsp.Position{Line: safeLineToLSP(mc.StartLine), Character: 0},
				End:   lsp.Position{Line: safeLineToLSP(mc.EndLine), Character: 0},
			},
		}, nil
	}

	return nil, nil
}

func buildModuleHoverMarkdown(mc scanner.ModuleCost, resources []scanner.ResourceResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### %s\n\n", mc.Name)
	fmt.Fprintf(&b, "**Monthly Cost:** %s/mo (%d %s)\n",
		scanner.FormatCost(mc.MonthlyCost),
		mc.ResourceCount,
		pluralize(mc.ResourceCount, "resource", "resources"),
	)

	if mc.ResourceCount > 3 {
		slices.SortFunc(resources, func(a, b scanner.ResourceResult) int {
			if b.MonthlyCost.GreaterThan(a.MonthlyCost) {
				return 1
			}
			if a.MonthlyCost.GreaterThan(b.MonthlyCost) {
				return -1
			}
			return 0
		})

		b.WriteString("\n**Top resources:**\n\n")
		b.WriteString("| Resource | Monthly |\n|:---|---:|\n")
		for _, r := range resources[:3] {
			// Strip the module prefix for brevity.
			name := strings.TrimPrefix(r.Name, mc.Name+".")
			fmt.Fprintf(&b, "| %s | %s |\n", name, scanner.FormatCost(r.MonthlyCost))
		}
	}

	return b.String()
}

func buildFullHoverMarkdown(r scanner.ResourceResult, violations []scanner.FinopsViolation, tagViolations []scanner.TagViolation) string {
	var b strings.Builder

	fmt.Fprintf(&b, "### %s\n\n", r.Name)
	fmt.Fprintf(&b, "**Monthly Cost:** %s/mo\n\n", scanner.FormatCost(r.MonthlyCost))

	if len(r.CostComponents) > 0 {
		b.WriteString("<details><summary>Cost Components</summary>\n\n")
		b.WriteString("| Component | Qty | Unit | Price | Monthly |\n")
		b.WriteString("|:---|---:|:---|---:|---:|\n")

		for _, c := range r.CostComponents {
			qty := "-"
			if c.MonthlyQuantity != nil && !c.MonthlyQuantity.IsZero() {
				qty = fmt.Sprintf("%.4g", c.MonthlyQuantity.Float64())
			}
			price := "-"
			if c.Price != nil && !c.Price.IsZero() {
				price = fmt.Sprintf("$%.4f", c.Price.Float64())
			}

			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
				c.Name, qty, c.Unit, price,
				scanner.FormatCost(c.TotalMonthlyCost),
			)
		}
		b.WriteString("\n</details>\n\n")
	}

	for i, v := range violations {
		b.WriteString("---\n\n")
		if len(violations) > 1 {
			fmt.Fprintf(&b, "#### FinOps Issue %d of %d\n\n", i+1, len(violations))
		} else {
			b.WriteString("#### FinOps Issue\n\n")
		}
		fmt.Fprintf(&b, "%s\n", v.Markdown)
	}

	for i, v := range tagViolations {
		b.WriteString("---\n\n")
		if len(tagViolations) > 1 {
			fmt.Fprintf(&b, "#### Tag Issue %d of %d\n\n", i+1, len(tagViolations))
		} else {
			b.WriteString("#### Tag Issue\n\n")
		}
		fmt.Fprintf(&b, "%s\n", v.Markdown)
	}

	return b.String()
}
