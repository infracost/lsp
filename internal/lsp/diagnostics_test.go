package lsp

import (
	"testing"

	"github.com/infracost/go-proto/pkg/rat"
	"github.com/owenrumney/go-lsp/lsp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/infracost/lsp/internal/scanner"
)

func TestFinopsViolationToDiagnostic(t *testing.T) {
	tests := []struct {
		name      string
		violation scanner.FinopsViolation
		wantSev   lsp.DiagnosticSeverity
		wantMsg   string
		wantLine  int // expected start line (0-based)
	}{
		{
			name: "basic violation",
			violation: scanner.FinopsViolation{
				PolicyName: "Use gp3 volumes",
				PolicySlug: "aws-gp2-volumes",
				Message:    "Use gp3 volumes instead of gp2",
				Address:    "aws_ebs_volume.data",
				StartLine:  10,
				EndLine:    15,
			},
			wantSev:  lsp.SeverityWarning,
			wantMsg:  "Use gp3 volumes instead of gp2",
			wantLine: 9,
		},
		{
			name: "block pull request maps to error severity",
			violation: scanner.FinopsViolation{
				PolicyName:       "Required tags",
				PolicySlug:       "required-tags",
				BlockPullRequest: true,
				Message:          "Required tag 'cost-center' is missing",
				StartLine:        5,
				EndLine:          8,
			},
			wantSev:  lsp.SeverityError,
			wantMsg:  "Required tag 'cost-center' is missing",
			wantLine: 4,
		},
		{
			name: "with monthly savings",
			violation: scanner.FinopsViolation{
				PolicyName:     "Use gp3 volumes",
				PolicySlug:     "aws-gp2-volumes",
				Message:        "Use gp3 volumes instead of gp2",
				MonthlySavings: mustRat("12.40"),
				StartLine:      1,
				EndLine:        3,
			},
			wantSev:  lsp.SeverityWarning,
			wantMsg:  "Use gp3 volumes instead of gp2 — saves $12.40/mo",
			wantLine: 0,
		},
		{
			name: "zero savings not shown",
			violation: scanner.FinopsViolation{
				PolicySlug:     "some-policy",
				Message:        "Fix this thing",
				MonthlySavings: rat.Zero,
				StartLine:      1,
				EndLine:        1,
			},
			wantSev:  lsp.SeverityWarning,
			wantMsg:  "Fix this thing",
			wantLine: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diags := finopsViolationToDiagnostic(tt.violation)

			require.Len(t, diags, 1)
			d := diags[0]

			require.NotNil(t, d.Severity)
			assert.Equal(t, tt.wantSev, *d.Severity, "severity")
			assert.Equal(t, tt.wantMsg, d.Message, "message")
			assert.Equal(t, "infracost", d.Source, "source")
			assert.Equal(t, tt.wantLine, d.Range.Start.Line, "start line")
		})
	}
}

func TestTagViolationToDiagnostics(t *testing.T) {
	tests := []struct {
		name      string
		violation scanner.TagViolation
		wantSev   lsp.DiagnosticSeverity
		wantMsgs  []string
		wantLine  int
	}{
		{
			name: "missing mandatory tags warning",
			violation: scanner.TagViolation{
				PolicyName:  "Required Tags",
				Address:     "aws_instance.web",
				Message:     "Missing mandatory tags: env, team",
				MissingTags: []string{"env", "team"},
				StartLine:   5,
				EndLine:     10,
			},
			wantSev:  lsp.SeverityWarning,
			wantMsgs: []string{"Missing tag: env", "Missing tag: team"},
			wantLine: 4,
		},
		{
			name: "blocking tag violation is error",
			violation: scanner.TagViolation{
				PolicyName:  "Strict Tags",
				BlockPR:     true,
				Address:     "aws_s3_bucket.data",
				Message:     "Missing mandatory tags: cost-center",
				MissingTags: []string{"cost-center"},
				StartLine:   1,
				EndLine:     3,
			},
			wantSev:  lsp.SeverityError,
			wantMsgs: []string{"Missing tag: cost-center"},
			wantLine: 0,
		},
		{
			name: "invalid tag value",
			violation: scanner.TagViolation{
				PolicyName: "Env Tag Policy",
				Address:    "aws_instance.web",
				Message:    "Invalid tag `env`: value `test`",
				InvalidTags: []scanner.InvalidTagResult{
					{Key: "env", Value: "test", Message: "Invalid tag `env`: value `test`"},
				},
				StartLine: 10,
				EndLine:   15,
			},
			wantSev:  lsp.SeverityWarning,
			wantMsgs: []string{"Invalid tag: env=test"},
			wantLine: 9,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diags := tagViolationToDiagnostics(tt.violation)

			require.Len(t, diags, len(tt.wantMsgs))

			for i, d := range diags {
				require.NotNil(t, d.Severity, "diag[%d]", i)
				assert.Equal(t, tt.wantSev, *d.Severity, "diag[%d]: severity", i)
				assert.Equal(t, tt.wantMsgs[i], d.Message, "diag[%d]: message", i)
				assert.Equal(t, "infracost", d.Source, "diag[%d]: source", i)
				assert.Equal(t, tt.wantLine, d.Range.Start.Line, "diag[%d]: start line", i)
			}
		})
	}
}
