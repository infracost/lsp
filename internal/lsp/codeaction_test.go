package lsp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/owenrumney/go-lsp/lsp"

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
				if fix != nil {
					t.Fatalf("expected nil, got %+v", fix)
				}
				return
			}

			if fix == nil {
				t.Fatal("expected non-nil fix")
			}
			if fix.Attribute != tt.wantAttr {
				t.Errorf("attribute = %q, want %q", fix.Attribute, tt.wantAttr)
			}
			if fix.OldValue != tt.wantOld {
				t.Errorf("oldValue = %q, want %q", fix.OldValue, tt.wantOld)
			}
			if fix.NewValue != tt.wantNew {
				t.Errorf("newValue = %q, want %q", fix.NewValue, tt.wantNew)
			}
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
	if err := os.WriteFile(tfFile, []byte(tfContent), 0644); err != nil {
		t.Fatal(err)
	}
	fileURI := "file://" + tfFile

	s := &Server{
		projectResults: map[string]*scanner.ScanResult{
			"test": {
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
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(actions) != tt.wantCount {
				t.Fatalf("got %d actions, want %d", len(actions), tt.wantCount)
			}

			if tt.wantCount > 0 {
				if actions[0].Title != tt.wantTitle {
					t.Errorf("title = %q, want %q", actions[0].Title, tt.wantTitle)
				}
				if tt.wantDisabled {
					if actions[0].Disabled == nil {
						t.Error("expected disabled action")
					}
				} else {
					if actions[0].Edit == nil {
						t.Error("expected edit on action")
					}
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
	if err := os.WriteFile(tfFile, []byte(tfContent), 0644); err != nil {
		t.Fatal(err)
	}
	fileURI := "file://" + tfFile

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
				if edit != nil {
					t.Fatal("expected nil edit")
				}
				return
			}

			if edit == nil {
				t.Fatal("expected non-nil edit")
			}

			edits := edit.Changes[lsp.DocumentURI(fileURI)]
			if len(edits) != 1 {
				t.Fatalf("expected 1 edit, got %d", len(edits))
			}

			te := edits[0]
			if te.NewText != tt.wantNewText {
				t.Errorf("newText = %q, want %q", te.NewText, tt.wantNewText)
			}
			if te.Range.Start.Line != tt.wantLine {
				t.Errorf("line = %d, want %d", te.Range.Start.Line, tt.wantLine)
			}
			if te.Range.Start.Character != tt.wantChar {
				t.Errorf("char = %d, want %d", te.Range.Start.Character, tt.wantChar)
			}
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
	if err := os.WriteFile(tfFile, []byte(tfContent), 0644); err != nil {
		t.Fatal(err)
	}
	fileURI := "file://" + tfFile

	// Simulates: Set `copy_tags_to_snapshot` = true (no old value from parser)
	fix := attributeFix{Attribute: "copy_tags_to_snapshot", NewValue: "true"}
	v := &scanner.FinopsViolation{Filename: tfFile, StartLine: 1, EndLine: 4}

	edit := buildAttributeEdit(fileURI, fix, v)
	if edit == nil {
		t.Fatal("expected non-nil edit")
	}

	edits := edit.Changes[lsp.DocumentURI(fileURI)]
	if len(edits) != 1 {
		t.Fatalf("expected 1 edit, got %d", len(edits))
	}

	if edits[0].NewText != "true" {
		t.Errorf("newText = %q, want %q", edits[0].NewText, "true")
	}
	if edits[0].Range.Start.Line != 1 {
		t.Errorf("line = %d, want 1", edits[0].Range.Start.Line)
	}
}

func TestBuildAttributeEditArrayIndex(t *testing.T) {
	dir := t.TempDir()
	tfFile := filepath.Join(dir, "main.tf")
	tfContent := `resource "aws_autoscaling_group" "web" {
  instance_types = ["t3.large"]
  min_size       = 1
}
`
	if err := os.WriteFile(tfFile, []byte(tfContent), 0644); err != nil {
		t.Fatal(err)
	}
	fileURI := "file://" + tfFile

	// Simulates: Switch `instance_types[0]` from `t3.large` to `t4g.large`
	fix := attributeFix{Attribute: "instance_types[0]", OldValue: "t3.large", NewValue: "t4g.large"}
	v := &scanner.FinopsViolation{Filename: tfFile, StartLine: 1, EndLine: 4}

	edit := buildAttributeEdit(fileURI, fix, v)
	if edit == nil {
		t.Fatal("expected non-nil edit")
	}

	edits := edit.Changes[lsp.DocumentURI(fileURI)]
	if len(edits) != 1 {
		t.Fatalf("expected 1 edit, got %d", len(edits))
	}

	if edits[0].NewText != "t4g.large" {
		t.Errorf("newText = %q, want %q", edits[0].NewText, "t4g.large")
	}
}
