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
	seenIgnore := make(map[string]struct{})

	slog.Debug("codeAction: request", "uri", uri,
		"diagnostics", len(params.Context.Diagnostics),
		"range_start", params.Range.Start.Line,
		"range_end", params.Range.End.Line,
	)

	// Filter diagnostics to only those matching the request range when
	// possible. When triggered from a specific diagnostic (lightbulb,
	// problems panel), params.Range matches that diagnostic's range exactly.
	diagnostics := filterDiagnosticsToRange(params.Context.Diagnostics, params.Range)

	for _, d := range diagnostics {
		slog.Debug("codeAction: diagnostic", "source", d.Source, "code", string(d.Code), "message", d.Message,
			"diag_start", d.Range.Start.Line, "diag_end", d.Range.End.Line,
			"params_start", params.Range.Start.Line, "params_end", params.Range.End.Line,
		)

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
			actions = append(actions, s.tagViolationCodeActions(uri, policyName, d, seenIgnore)...)
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
				if v.MonthlySavings != nil && !v.MonthlySavings.IsZero() && s.resourceHasCost(v.Address) {
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
			}
		} else {
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

		absPath, _ := filepath.Abs(v.Filename)
		actions = append(actions, deduplicatedDismissActions(d, absPath, v.Address, code, seenIgnore)...)
	}

	return actions, nil
}

// tagViolationCodeActions builds code actions for a tag policy diagnostic.
func (s *Server) tagViolationCodeActions(uri, policyName string, d lsp.Diagnostic, seenIgnore map[string]struct{}) []lsp.CodeAction {
	v := s.findTagViolation(uri, policyName, d.Range)
	if v == nil {
		slog.Debug("codeAction: no matching tag violation", "uri", uri, "policy", policyName, "line", d.Range.Start.Line)
		return nil
	}

	var actions []lsp.CodeAction

	// Match the diagnostic message back to the specific invalid tag.
	for _, it := range v.InvalidTags {
		if it.Suggestion == "" {
			continue
		}
		// Only offer a fix for the invalid tag that matches this diagnostic.
		if !strings.Contains(d.Message, it.Key+"="+it.Value) {
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
			Title:       d.Message,
			Kind:        &kind,
			Diagnostics: []lsp.Diagnostic{d},
			Disabled: &struct {
				Reason string `json:"reason"`
			}{
				Reason: "Requires manual changes",
			},
		})
	}

	absPath, _ := filepath.Abs(v.Filename)
	slug := tagDiagnosticSlug(v.PolicyName, d.Message)
	actions = append(actions, deduplicatedDismissActions(d, absPath, v.Address, slug, seenIgnore)...)

	return actions
}

// tagDiagnosticSlug builds a unique slug for a specific tag diagnostic by
// combining the policy name with the diagnostic message. This ensures that
// "Missing tag: Environment" and "Missing tag: Team" under the same policy
// get distinct ignore entries.
func tagDiagnosticSlug(policyName, message string) string {
	return "tag:" + policyName + ":" + message
}

// findTagViolation matches a diagnostic back to a cached TagViolation.
func (s *Server) findTagViolation(uri, policyName string, rng lsp.Range) *scanner.TagViolation {
	result := s.getMergedResult()
	reqPath := filepath.Clean(uriToPath(uri))

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
	reqPath := filepath.Clean(uriToPath(uri))

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
	filePath := filepath.Clean(uriToPath(uri))
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

// deduplicatedDismissActions returns dismiss code actions, skipping any whose
// slug has already been seen. This prevents duplicate entries when the client
// sends multiple diagnostics for the same resource range.
func deduplicatedDismissActions(d lsp.Diagnostic, absPath, resource, slug string, seen map[string]struct{}) []lsp.CodeAction {
	if _, ok := seen[slug]; ok {
		return nil
	}
	seen[slug] = struct{}{}

	kind := lsp.CodeActionQuickFix

	marshalArg := func(v string) json.RawMessage {
		b, _ := json.Marshal(v)
		return b
	}

	return []lsp.CodeAction{
		{
			Title:       fmt.Sprintf("Dismiss '%s' for this resource", dismissLabel(slug)),
			Kind:        &kind,
			Diagnostics: []lsp.Diagnostic{d},
			Command: &lsp.Command{
				Title:   "Dismiss diagnostic",
				Command: "infracost.dismissDiagnostic",
				Arguments: []json.RawMessage{
					marshalArg(absPath),
					marshalArg(resource),
					marshalArg(slug),
				},
			},
		},
		{
			Title:       fmt.Sprintf("Dismiss '%s' everywhere", dismissLabel(slug)),
			Kind:        &kind,
			Diagnostics: []lsp.Diagnostic{d},
			Command: &lsp.Command{
				Title:   "Dismiss diagnostic globally",
				Command: "infracost.dismissDiagnostic",
				Arguments: []json.RawMessage{
					marshalArg("*"),
					marshalArg("*"),
					marshalArg(slug),
				},
			},
		},
	}
}

// dismissLabel returns a human-readable label for a dismiss action title.
// For tag slugs of the form "tag:policyName:message", it extracts just the
// message portion. For all other slugs it returns them unchanged.
func dismissLabel(slug string) string {
	if after, ok := strings.CutPrefix(slug, "tag:"); ok {
		if _, msg, found := strings.Cut(after, ":"); found {
			return msg
		}
	}
	return slug
}

// filterDiagnosticsToRange returns only the diagnostics whose range exactly
// matches r. If no diagnostics match exactly (e.g. when triggered from a
// cursor position rather than a specific diagnostic), all are returned.
func filterDiagnosticsToRange(diagnostics []lsp.Diagnostic, r lsp.Range) []lsp.Diagnostic {
	var matched []lsp.Diagnostic
	for _, d := range diagnostics {
		if d.Range.Start.Line == r.Start.Line && d.Range.End.Line == r.End.Line &&
			d.Range.Start.Character == r.Start.Character && d.Range.End.Character == r.End.Character {
			matched = append(matched, d)
		}
	}
	if len(matched) > 0 {
		return matched
	}
	return diagnostics
}

// resourceHasCost returns true if the resource with the given address has a
// non-zero monthly cost. Used to suppress savings labels for free/unpriced resources.
func (s *Server) resourceHasCost(address string) bool {
	for _, r := range s.getMergedResult().Resources {
		if r.Name == address {
			return r.MonthlyCost != nil && !r.MonthlyCost.IsZero()
		}
	}
	return false
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
