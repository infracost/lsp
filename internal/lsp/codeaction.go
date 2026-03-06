package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/owenrumney/go-lsp/lsp"

	"github.com/infracost/lsp/internal/scanner"
)

// attributeFix describes a simple attribute value replacement parsed from a
// violation description.
type attributeFix struct {
	Attribute string // HCL attribute name (e.g. "volume_type")
	OldValue  string // current value to match, empty if unknown
	NewValue  string // replacement value (e.g. `"gp3"`, "true")
}

// parseAttributeFix attempts to extract an attribute fix from a violation's
// description. Returns nil if the description doesn't match a known pattern.
//
// Supported patterns from provider policy descriptions:
//
//	Switch `instance_type` from `t3.medium` to `t4g.medium`
//	Set `copy_tags_to_snapshot` = true
//	Set `associate_public_ip_address = false`
func parseAttributeFix(description string) *attributeFix {
	// Pattern 1: Switch `attr` from `old` to `new`
	if m := switchPattern.FindStringSubmatch(description); m != nil {
		return &attributeFix{
			Attribute: m[1],
			OldValue:  m[2],
			NewValue:  m[3],
		}
	}

	// Pattern 2: Set `attr` = value  (e.g. Set `copy_tags_to_snapshot` = true)
	if m := setEqualsPattern.FindStringSubmatch(description); m != nil {
		return &attributeFix{
			Attribute: m[1],
			NewValue:  m[2],
		}
	}

	// Pattern 3: Set `attr = value`  (e.g. Set `associate_public_ip_address = false`)
	if m := setInlinePattern.FindStringSubmatch(description); m != nil {
		return &attributeFix{
			Attribute: m[1],
			NewValue:  m[2],
		}
	}

	return nil
}

var (
	// Switch `attr` from `old` to `new`
	switchPattern = regexp.MustCompile("Switch `([^`]+)` from `([^`]+)` to `([^`]+)`")

	// Set `attr` = value
	setEqualsPattern = regexp.MustCompile("Set `([^`]+)` = ([^ ]+)")

	// Set `attr = value`
	setInlinePattern = regexp.MustCompile("Set `([^=`]+?)\\s*=\\s*([^`]+?)`")
)

// CodeAction implements server.CodeActionHandler.
func (s *Server) CodeAction(_ context.Context, params *lsp.CodeActionParams) ([]lsp.CodeAction, error) {
	uri := string(params.TextDocument.URI)
	actions := make([]lsp.CodeAction, 0, len(params.Context.Diagnostics))

	slog.Debug("codeAction: request", "uri", uri, "diagnostics", len(params.Context.Diagnostics))

	for _, d := range params.Context.Diagnostics {
		slog.Debug("codeAction: diagnostic", "source", d.Source, "code", string(d.Code), "message", d.Message, "line", d.Range.Start.Line)

		if d.Source != "infracost" {
			continue
		}

		code := diagnosticPolicySlug(d)
		if code == "" {
			slog.Debug("codeAction: empty code", "code", string(d.Code))
			continue
		}

		// Tag policy diagnostics have codes prefixed with "tag:".
		if policyName, ok := strings.CutPrefix(code, "tag:"); ok {
			actions = append(actions, s.tagViolationCodeActions(uri, policyName, d)...)
			continue
		}

		// FinOps policy diagnostics use the policy slug as the code.
		v := s.findViolation(uri, code, d.Range)
		if v == nil {
			slog.Debug("codeAction: no matching violation", "uri", uri, "slug", code, "line", d.Range.Start.Line)
			continue
		}

		fix := parseAttributeFix(v.Message)
		slog.Debug("codeAction: parsed fix", "message", v.Message, "fix", fix)

		// If we can parse a concrete fix, try to build an edit.
		if fix != nil {
			if edit := buildAttributeEdit(uri, *fix, v); edit != nil {
				title := v.Message
				if v.MonthlySavings != nil && !v.MonthlySavings.IsZero() {
					title += fmt.Sprintf(" — saves %s/mo", scanner.FormatCost(v.MonthlySavings))
				}

				kind := lsp.CodeActionQuickFix
				actions = append(actions, lsp.CodeAction{
					Title:       title,
					Kind:        &kind,
					Diagnostics: []lsp.Diagnostic{d},
					IsPreferred: ptrTo(true),
					Edit:        edit,
				})
				continue
			}
		}

		// No auto-fix available. Return a disabled action with the
		// violation description so coding agents can see the suggested fix.
		kind := lsp.CodeActionQuickFix
		actions = append(actions, lsp.CodeAction{
			Title:       v.Message,
			Kind:        &kind,
			Diagnostics: []lsp.Diagnostic{d},
			Disabled: &struct {
				Reason string `json:"reason"`
			}{
				Reason: "Requires manual changes",
			},
		})
	}

	return actions, nil
}

// tagViolationCodeActions builds code actions for a tag policy diagnostic.
func (s *Server) tagViolationCodeActions(uri, policyName string, d lsp.Diagnostic) []lsp.CodeAction {
	v := s.findTagViolation(uri, policyName, d.Range)
	if v == nil {
		slog.Debug("codeAction: no matching tag violation", "uri", uri, "policy", policyName, "line", d.Range.Start.Line)
		return nil
	}

	var actions []lsp.CodeAction

	// For invalid tags with a suggestion, offer a replacement edit.
	for _, it := range v.InvalidTags {
		if it.Suggestion == "" {
			continue
		}
		fix := attributeFix{
			Attribute: it.Key,
			OldValue:  it.Value,
			NewValue:  it.Suggestion,
		}
		loc := &scanner.FinopsViolation{
			StartLine: v.StartLine,
			EndLine:   v.EndLine,
		}
		if edit := buildAttributeEdit(uri, fix, loc); edit != nil {
			kind := lsp.CodeActionQuickFix
			actions = append(actions, lsp.CodeAction{
				Title:       fmt.Sprintf("Change tag `%s` to `%s`", it.Key, it.Suggestion),
				Kind:        &kind,
				Diagnostics: []lsp.Diagnostic{d},
				IsPreferred: ptrTo(true),
				Edit:        edit,
			})
		}
	}

	// If no auto-fix was generated, return a disabled action.
	if len(actions) == 0 {
		kind := lsp.CodeActionQuickFix
		actions = append(actions, lsp.CodeAction{
			Title:       v.Message,
			Kind:        &kind,
			Diagnostics: []lsp.Diagnostic{d},
			Disabled: &struct {
				Reason string `json:"reason"`
			}{
				Reason: "Requires manual changes",
			},
		})
	}

	return actions
}

// findTagViolation matches a diagnostic back to a cached TagViolation.
func (s *Server) findTagViolation(uri, policyName string, rng lsp.Range) *scanner.TagViolation {
	result := s.getMergedResult()
	reqPath := uriToPath(uri)

	for i, v := range result.TagViolations {
		if v.PolicyName != policyName {
			continue
		}

		absPath, err := filepath.Abs(v.Filename)
		if err != nil || absPath != reqPath {
			continue
		}

		if safeLineToLSP(v.StartLine) == rng.Start.Line {
			return &result.TagViolations[i]
		}
	}

	return nil
}

// findViolation matches a diagnostic back to a cached FinopsViolation.
func (s *Server) findViolation(uri, slug string, rng lsp.Range) *scanner.FinopsViolation {
	result := s.getMergedResult()
	reqPath := uriToPath(uri)

	for i, v := range result.Violations {
		if v.PolicySlug != slug {
			continue
		}

		absPath, err := filepath.Abs(v.Filename)
		if err != nil || absPath != reqPath {
			continue
		}

		if safeLineToLSP(v.StartLine) == rng.Start.Line {
			return &result.Violations[i]
		}
	}

	return nil
}

// buildAttributeEdit reads the file and builds a WorkspaceEdit that replaces
// the attribute value within the violation's line range.
func buildAttributeEdit(uri string, fix attributeFix, v *scanner.FinopsViolation) *lsp.WorkspaceEdit {
	filePath := uriToPath(uri)
	content, err := os.ReadFile(filePath) //nolint:gosec // path derived from LSP document URI
	if err != nil {
		slog.Warn("buildAttributeEdit: failed to read file", "path", filePath, "error", err)
		return nil
	}

	lines := strings.Split(string(content), "\n")

	startLine := int(v.StartLine - 1) // convert to 0-based
	endLine := int(v.EndLine - 1)
	if startLine < 0 {
		startLine = 0
	}
	if endLine >= len(lines) {
		endLine = len(lines) - 1
	}

	// Strip array index from attribute name for line matching.
	// e.g. "instance_types[0]" → "instance_types"
	attrName := fix.Attribute
	if idx := strings.Index(attrName, "["); idx >= 0 {
		attrName = attrName[:idx]
	}

	for i := startLine; i <= endLine; i++ {
		line := lines[i]

		attrIdx := strings.Index(line, attrName)
		if attrIdx < 0 {
			continue
		}

		// Ensure attribute name is followed by `=` (with optional whitespace).
		rest := strings.TrimSpace(line[attrIdx+len(attrName):])
		if !strings.HasPrefix(rest, "=") {
			continue
		}

		// Find the value to replace. If OldValue is set, match it exactly.
		// Otherwise, find the value after `=` and replace it with NewValue.
		if fix.OldValue != "" {
			oldIdx := strings.Index(line, fix.OldValue)
			if oldIdx < 0 {
				continue
			}
			return makeEdit(uri, i, oldIdx, len(fix.OldValue), fix.NewValue)
		}

		// No old value — find the value portion after `=`.
		eqIdx := strings.Index(line[attrIdx:], "=")
		if eqIdx < 0 {
			continue
		}

		valueStart := attrIdx + eqIdx + 1
		valuePart := line[valueStart:]
		trimmed := strings.TrimLeft(valuePart, " \t")
		leadingSpace := len(valuePart) - len(trimmed)
		valueStartChar := valueStart + leadingSpace

		// Determine the extent of the current value.
		valueLen := len(strings.TrimRight(trimmed, " \t"))
		if valueLen == 0 {
			continue
		}

		return makeEdit(uri, i, valueStartChar, valueLen, fix.NewValue)
	}

	slog.Debug("buildAttributeEdit: attribute not found in range",
		"attribute", fix.Attribute,
		"startLine", v.StartLine,
		"endLine", v.EndLine,
	)
	return nil
}

func makeEdit(uri string, line, startChar, length int, newText string) *lsp.WorkspaceEdit {
	return &lsp.WorkspaceEdit{
		Changes: map[lsp.DocumentURI][]lsp.TextEdit{
			lsp.DocumentURI(uri): {
				{
					Range: lsp.Range{
						Start: lsp.Position{Line: line, Character: startChar},
						End:   lsp.Position{Line: line, Character: startChar + length},
					},
					NewText: newText,
				},
			},
		},
	}
}

// diagnosticPolicySlug extracts the policy slug from a diagnostic's Code field.
func diagnosticPolicySlug(d lsp.Diagnostic) string {
	if d.Code == nil {
		return ""
	}
	var slug string
	if err := json.Unmarshal(d.Code, &slug); err != nil {
		return ""
	}
	return slug
}
