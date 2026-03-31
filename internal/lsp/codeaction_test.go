package lsp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/owenrumney/go-lsp/lsp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/infracost/lsp/internal/api"
	"github.com/infracost/lsp/internal/scanner"
)

func TestParseAttributeFix(t *testing.T) {
	tests := []struct {
		name        string
		description string
		wantNil     bool
		wantAttr    string
		wantOld     string
		wantNew     string
	}{
		{
			name:        "switch from to",
			description: "Switch `instance_type` from `t3.medium` to `t4g.medium`",
			wantAttr:    "instance_type",
			wantOld:     "t3.medium",
			wantNew:     "t4g.medium",
		},
		{
			name:        "switch with array index",
			description: "Switch `instance_types[0]` from `t3.large` to `t4g.large`",
			wantAttr:    "instance_types[0]",
			wantOld:     "t3.large",
			wantNew:     "t4g.large",
		},
		{
			name:        "set equals",
			description: "Set `copy_tags_to_snapshot` = true",
			wantAttr:    "copy_tags_to_snapshot",
			wantNew:     "true",
		},
		{
			name:        "set inline",
			description: "Consider disabling `associate_public_ip_address`. Set `associate_public_ip_address = false` and use NAT gateways.",
			wantAttr:    "associate_public_ip_address",
			wantNew:     "false",
		},
		{
			name:        "no match — add resource",
			description: "Add a `aws_s3_bucket_lifecycle_configuration` resource to define a `abort_incomplete_multipart_upload` rule",
			wantNil:     true,
		},
		{
			name:        "no match — set to include",
			description: "Set `architectures` to include `arm64`",
			wantNil:     true,
		},
		{
			name:        "no match — freeform",
			description: "Consider enabling CloudWatch log exports. Set `enabled_cloudwatch_logs_exports` to include the right log types.",
			wantNil:     true,
		},
		{
			name:        "empty description",
			description: "",
			wantNil:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fix := parseAttributeFix(tt.description)

			if tt.wantNil {
				require.Nil(t, fix)
				return
			}

			require.NotNil(t, fix)
			assert.Equal(t, tt.wantAttr, fix.Attribute, "attribute")
			assert.Equal(t, tt.wantOld, fix.OldValue, "oldValue")
			assert.Equal(t, tt.wantNew, fix.NewValue, "newValue")
		})
	}
}

func TestCodeAction(t *testing.T) {
	dir := t.TempDir()
	tfFile := filepath.Join(dir, "main.tf")
	tfContent := `resource "aws_instance" "web" {
  ami           = "ami-12345"
  instance_type = "t3.medium"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(tfContent), 0644))
	fileURI := "file://" + tfFile

	s := NewServer(nil, nil, api.NewTokenSource(nil))
	s.projectResults["test"] = &scanner.ScanResult{
		Violations: []scanner.FinopsViolation{
			{
				PolicySlug:     "aws-use-graviton-ec2-instance",
				Message:        "Switch `instance_type` from `t3.medium` to `t4g.medium`",
				Address:        "aws_instance.web",
				Filename:       tfFile,
				StartLine:      1,
				EndLine:        4,
				MonthlySavings: mustRat("5.00"),
			},
			{
				PolicySlug: "aws-use-ecr-lifecycle-policy",
				Message:    "Add a `aws_ecr_lifecycle_policy` resource to define image lifecycle rules",
				Address:    "aws_instance.web",
				Filename:   tfFile,
				StartLine:  1,
				EndLine:    4,
			},
		},
	}

	tests := []struct {
		name         string
		diagnostics  []lsp.Diagnostic
		wantCount    int
		wantTitle    string
		wantDisabled bool
	}{
		{
			name: "parseable description produces edit action plus ignore actions",
			diagnostics: []lsp.Diagnostic{
				{
					Source:  "infracost",
					Code:    mustMarshal("aws-use-graviton-ec2-instance"),
					Message: "Switch `instance_type` from `t3.medium` to `t4g.medium`",
					Range:   lsp.Range{Start: lsp.Position{Line: 0}},
				},
			},
			wantCount: 3,
			wantTitle: "Switch `instance_type` from `t3.medium` to `t4g.medium` — saves $5.00/mo",
		},
		{
			name: "unparseable description produces disabled action plus ignore actions",
			diagnostics: []lsp.Diagnostic{
				{
					Source:  "infracost",
					Code:    mustMarshal("aws-use-ecr-lifecycle-policy"),
					Message: "Add a `aws_ecr_lifecycle_policy` resource",
					Range:   lsp.Range{Start: lsp.Position{Line: 0}},
				},
			},
			wantCount:    3,
			wantTitle:    "Add a `aws_ecr_lifecycle_policy` resource to define image lifecycle rules",
			wantDisabled: true,
		},
		{
			name: "unknown slug produces no action",
			diagnostics: []lsp.Diagnostic{
				{
					Source: "infracost",
					Code:   mustMarshal("unknown-policy"),
					Range:  lsp.Range{Start: lsp.Position{Line: 0}},
				},
			},
			wantCount: 0,
		},
		{
			name: "non-infracost diagnostic ignored",
			diagnostics: []lsp.Diagnostic{
				{Source: "terraform-ls"},
			},
			wantCount: 0,
		},
		{
			name:        "empty diagnostics",
			diagnostics: nil,
			wantCount:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := &lsp.CodeActionParams{
				TextDocument: lsp.TextDocumentIdentifier{URI: lsp.DocumentURI(fileURI)},
				Context:      lsp.CodeActionContext{Diagnostics: tt.diagnostics},
			}

			actions, err := s.CodeAction(context.Background(), params)
			require.NoError(t, err)
			require.Len(t, actions, tt.wantCount)

			if tt.wantCount > 0 {
				assert.Equal(t, tt.wantTitle, actions[0].Title, "title")
				if tt.wantDisabled {
					assert.NotNil(t, actions[0].Disabled, "expected disabled action")
				} else {
					assert.NotNil(t, actions[0].Edit, "expected edit on action")
				}
			}
		})
	}
}

func TestBuildAttributeEdit(t *testing.T) {
	dir := t.TempDir()
	tfFile := filepath.Join(dir, "main.tf")
	tfContent := `resource "aws_instance" "web" {
  ami           = "ami-12345"
  instance_type = "t3.medium"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(tfContent), 0644))
	fileURI := pathToURI(tfFile)

	tests := []struct {
		name        string
		fix         attributeFix
		violation   scanner.FinopsViolation
		wantNil     bool
		wantNewText string
		wantLine    int
		wantChar    int
	}{
		{
			name: "switch with old value match",
			fix:  attributeFix{Attribute: "instance_type", OldValue: `"t3.medium"`, NewValue: `"t4g.medium"`},
			violation: scanner.FinopsViolation{
				Filename: tfFile, StartLine: 1, EndLine: 4,
			},
			wantNewText: `"t4g.medium"`,
			wantLine:    2,
			wantChar:    18,
		},
		{
			name: "set without old value",
			fix:  attributeFix{Attribute: "ami", NewValue: `"ami-99999"`},
			violation: scanner.FinopsViolation{
				Filename: tfFile, StartLine: 1, EndLine: 4,
			},
			wantNewText: `"ami-99999"`,
			wantLine:    1,
			wantChar:    18,
		},
		{
			name: "attribute not in range",
			fix:  attributeFix{Attribute: "instance_type", OldValue: `"t3.medium"`, NewValue: `"t4g.medium"`},
			violation: scanner.FinopsViolation{
				Filename: tfFile, StartLine: 1, EndLine: 2,
			},
			wantNil: true,
		},
		{
			name: "wrong old value",
			fix:  attributeFix{Attribute: "instance_type", OldValue: `"m5.large"`, NewValue: `"m6g.large"`},
			violation: scanner.FinopsViolation{
				Filename: tfFile, StartLine: 1, EndLine: 4,
			},
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			edit := buildAttributeEdit(fileURI, tt.fix, &tt.violation)

			if tt.wantNil {
				require.Nil(t, edit)
				return
			}

			require.NotNil(t, edit)

			edits := edit.Changes[lsp.DocumentURI(fileURI)]
			require.Len(t, edits, 1)

			te := edits[0]
			assert.Equal(t, tt.wantNewText, te.NewText, "newText")
			assert.Equal(t, tt.wantLine, te.Range.Start.Line, "line")
			assert.Equal(t, tt.wantChar, te.Range.Start.Character, "char")
		})
	}
}

func TestBuildAttributeEditBooleans(t *testing.T) {
	dir := t.TempDir()
	tfFile := filepath.Join(dir, "main.tf")
	tfContent := `resource "aws_rds_cluster" "db" {
  copy_tags_to_snapshot = false
  engine                = "aurora-mysql"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(tfContent), 0644))
	fileURI := pathToURI(tfFile)

	// Simulates: Set `copy_tags_to_snapshot` = true (no old value from parser)
	fix := attributeFix{Attribute: "copy_tags_to_snapshot", NewValue: "true"}
	v := &scanner.FinopsViolation{Filename: tfFile, StartLine: 1, EndLine: 4}

	edit := buildAttributeEdit(fileURI, fix, v)
	require.NotNil(t, edit)

	edits := edit.Changes[lsp.DocumentURI(fileURI)]
	require.Len(t, edits, 1)

	assert.Equal(t, "true", edits[0].NewText, "newText")
	assert.Equal(t, 1, edits[0].Range.Start.Line, "line")
}

func TestBuildAttributeEditArrayIndex(t *testing.T) {
	dir := t.TempDir()
	tfFile := filepath.Join(dir, "main.tf")
	tfContent := `resource "aws_autoscaling_group" "web" {
  instance_types = ["t3.large"]
  min_size       = 1
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(tfContent), 0644))
	fileURI := pathToURI(tfFile)

	// Simulates: Switch `instance_types[0]` from `t3.large` to `t4g.large`
	fix := attributeFix{Attribute: "instance_types[0]", OldValue: "t3.large", NewValue: "t4g.large"}
	v := &scanner.FinopsViolation{Filename: tfFile, StartLine: 1, EndLine: 4}

	edit := buildAttributeEdit(fileURI, fix, v)
	require.NotNil(t, edit)

	edits := edit.Changes[lsp.DocumentURI(fileURI)]
	require.Len(t, edits, 1)

	assert.Equal(t, "t4g.large", edits[0].NewText, "newText")
}
