package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/infracost/lsp/internal/dashboard"
	"github.com/infracost/lsp/internal/scanner"
)

// Response types for infracost/resourceDetails.

type ResourceDetailsResult struct {
	Resource   *ResourceDetail `json:"resource,omitempty"`
	Scanning   bool            `json:"scanning"`
	NeedsLogin bool            `json:"needsLogin,omitempty"`
}

type ResourceDetail struct {
	Name           string                `json:"name"`
	Type           string                `json:"type"`
	MonthlyCost    string                `json:"monthlyCost"`
	CostComponents []CostComponentDetail `json:"costComponents"`
	Violations     []ViolationDetail     `json:"violations"`
	TagViolations  []TagViolationDetail  `json:"tagViolations"`
}

type CostComponentDetail struct {
	Name            string `json:"name"`
	Unit            string `json:"unit"`
	Price           string `json:"price"`
	MonthlyQuantity string `json:"monthlyQuantity"`
	MonthlyCost     string `json:"monthlyCost"`
}

type ViolationDetail struct {
	PolicyName       string                  `json:"policyName"`
	PolicySlug       string                  `json:"policySlug"`
	Message          string                  `json:"message"`
	Attribute        string                  `json:"attribute,omitempty"`
	BlockPullRequest bool                    `json:"blockPullRequest"`
	MonthlySavings   string                  `json:"monthlySavings,omitempty"`
	SavingsDetails   string                  `json:"savingsDetails,omitempty"`
	PolicyDetail     *dashboard.PolicyDetail `json:"policyDetail,omitempty"`
}

type TagViolationDetail struct {
	PolicyName    string             `json:"policyName"`
	BlockPR       bool               `json:"blockPR"`
	Message       string             `json:"message"`
	PolicyMessage string             `json:"policyMessage,omitempty"`
	MissingTags   []string           `json:"missingTags,omitempty"`
	InvalidTags   []InvalidTagDetail `json:"invalidTags,omitempty"`
}

type InvalidTagDetail struct {
	Key         string   `json:"key"`
	Value       string   `json:"value"`
	Suggestion  string   `json:"suggestion,omitempty"`
	Message     string   `json:"message,omitempty"`
	ValidValues []string `json:"validValues,omitempty"`
}

type resourceDetailsParams struct {
	URI  string `json:"uri"`
	Line int    `json:"line"`
}

// HandleResourceDetails handles the infracost/resourceDetails custom request.
func (s *Server) HandleResourceDetails(_ context.Context, params json.RawMessage) (any, error) {
	var p resourceDetailsParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}

	slog.Debug("resourceDetails: request", "uri", p.URI, "line", p.Line)

	needsLogin := !s.scanner.HasTokenSource()

	if s.isScanning() {
		return ResourceDetailsResult{Scanning: true, NeedsLogin: needsLogin}, nil
	}

	result := s.getMergedResult()
	if len(result.Resources) == 0 {
		return ResourceDetailsResult{NeedsLogin: needsLogin}, nil
	}

	reqPath := filepath.Clean(uriToPath(p.URI))
	line := int64(p.Line) + 1 // LSP is 0-based, our data is 1-based

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

		detail := buildResourceDetail(r, violationsByAddr[r.Name], tagViolationsByAddr[r.Name])
		return ResourceDetailsResult{Resource: &detail, NeedsLogin: needsLogin}, nil
	}

	return ResourceDetailsResult{NeedsLogin: needsLogin}, nil
}

func buildResourceDetail(r scanner.ResourceResult, violations []scanner.FinopsViolation, tagViolations []scanner.TagViolation) ResourceDetail {
	detail := ResourceDetail{
		Name:        r.Name,
		Type:        r.Type,
		MonthlyCost: scanner.FormatCost(r.MonthlyCost),
	}

	for _, c := range r.CostComponents {
		qty := "-"
		if c.MonthlyQuantity != nil && !c.MonthlyQuantity.IsZero() {
			qty = fmt.Sprintf("%.4g", c.MonthlyQuantity.Float64())
		}
		price := "-"
		if c.Price != nil && !c.Price.IsZero() {
			price = fmt.Sprintf("$%.4f", c.Price.Float64())
		}

		detail.CostComponents = append(detail.CostComponents, CostComponentDetail{
			Name:            c.Name,
			Unit:            c.Unit,
			Price:           price,
			MonthlyQuantity: qty,
			MonthlyCost:     scanner.FormatCost(c.TotalMonthlyCost),
		})
	}

	for _, v := range violations {
		vd := ViolationDetail{
			PolicyName:       v.PolicyName,
			PolicySlug:       v.PolicySlug,
			Message:          v.Message,
			Attribute:        v.Attribute,
			BlockPullRequest: v.BlockPullRequest,
			MonthlySavings:   scanner.FormatCost(v.MonthlySavings),
			SavingsDetails:   v.SavingsDetails,
		}
		if v.PolicyDetail != nil {
			vd.PolicyDetail = v.PolicyDetail
		}
		detail.Violations = append(detail.Violations, vd)
	}

	for _, v := range tagViolations {
		td := TagViolationDetail{
			PolicyName:    v.PolicyName,
			BlockPR:       v.BlockPR,
			Message:       v.Message,
			PolicyMessage: v.PolicyMessage,
			MissingTags:   v.MissingTags,
		}
		for _, it := range v.InvalidTags {
			td.InvalidTags = append(td.InvalidTags, InvalidTagDetail{
				Key:         it.Key,
				Value:       it.Value,
				Suggestion:  it.Suggestion,
				Message:     it.Message,
				ValidValues: it.ValidValues,
			})
		}
		detail.TagViolations = append(detail.TagViolations, td)
	}

	return detail
}
