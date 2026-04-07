package lsp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/infracost/lsp/internal/api"
	"github.com/infracost/lsp/internal/scanner"
)

func TestHandleFileSummary(t *testing.T) {
	dir := t.TempDir()
	tfFile := filepath.Join(dir, "main.tf")
	fileURI := pathToURI(tfFile)

	tests := []struct {
		name      string
		scanning  bool
		results   map[string]*scanner.ScanResult
		wantCount int
		wantFirst *FileSummaryResource
	}{
		{
			name:      "empty results",
			results:   map[string]*scanner.ScanResult{},
			wantCount: 0,
		},
		{
			name:     "scanning returns empty",
			scanning: true,
			results: map[string]*scanner.ScanResult{
				"proj": {
					Resources: []scanner.ResourceResult{
						{Name: "aws_instance.web", Filename: tfFile, StartLine: 1, EndLine: 4, MonthlyCost: mustRat("10.50")},
					},
				},
			},
			wantCount: 0,
		},
		{
			name: "single resource",
			results: map[string]*scanner.ScanResult{
				"proj": {
					Resources: []scanner.ResourceResult{
						{Name: "aws_instance.web", Filename: tfFile, StartLine: 5, EndLine: 10, MonthlyCost: mustRat("25.00")},
					},
				},
			},
			wantCount: 1,
			wantFirst: &FileSummaryResource{
				Name:        "aws_instance.web",
				Line:        4, // 0-based
				MonthlyCost: "$25.00",
			},
		},
		{
			name: "resource in different file excluded",
			results: map[string]*scanner.ScanResult{
				"proj": {
					Resources: []scanner.ResourceResult{
						{Name: "aws_instance.web", Filename: tfFile, StartLine: 1, EndLine: 4, MonthlyCost: mustRat("10.00")},
						{Name: "aws_s3_bucket.logs", Filename: filepath.Join(dir, "other.tf"), StartLine: 1, EndLine: 3},
					},
				},
			},
			wantCount: 1,
			wantFirst: &FileSummaryResource{
				Name:        "aws_instance.web",
				Line:        0,
				MonthlyCost: "$10.00",
			},
		},
		{
			name: "resource with empty filename excluded",
			results: map[string]*scanner.ScanResult{
				"proj": {
					Resources: []scanner.ResourceResult{
						{Name: "aws_instance.web", Filename: "", StartLine: 1, EndLine: 4},
						{Name: "aws_s3_bucket.data", Filename: tfFile, StartLine: 1, EndLine: 3, MonthlyCost: mustRat("0.50")},
					},
				},
			},
			wantCount: 1,
			wantFirst: &FileSummaryResource{
				Name:        "aws_s3_bucket.data",
				Line:        0,
				MonthlyCost: "$0.50",
			},
		},
		{
			name: "policy violations counted",
			results: map[string]*scanner.ScanResult{
				"proj": {
					Resources: []scanner.ResourceResult{
						{Name: "aws_instance.web", Filename: tfFile, StartLine: 1, EndLine: 4, MonthlyCost: mustRat("10.00")},
					},
					Violations: []scanner.FinopsViolation{
						{Address: "aws_instance.web", Filename: tfFile, PolicySlug: "use-graviton"},
						{Address: "aws_instance.web", Filename: tfFile, PolicySlug: "use-gp3"},
						{Address: "aws_instance.web", Filename: filepath.Join(dir, "other.tf"), PolicySlug: "other-file"},
					},
				},
			},
			wantCount: 1,
			wantFirst: &FileSummaryResource{
				Name:         "aws_instance.web",
				Line:         0,
				MonthlyCost:  "$10.00",
				PolicyIssues: 2,
			},
		},
		{
			name: "tag violations counted",
			results: map[string]*scanner.ScanResult{
				"proj": {
					Resources: []scanner.ResourceResult{
						{Name: "aws_instance.web", Filename: tfFile, StartLine: 1, EndLine: 4},
					},
					TagViolations: []scanner.TagViolation{
						{
							Address:     "aws_instance.web",
							Filename:    tfFile,
							PolicyName:  "required-tags",
							MissingTags: []string{"env", "team"},
						},
						{
							Address:    "aws_instance.web",
							Filename:   tfFile,
							PolicyName: "valid-tags",
							InvalidTags: []scanner.InvalidTagResult{
								{Key: "env", Value: "staging", Suggestion: "stg"},
							},
						},
					},
				},
			},
			wantCount: 1,
			wantFirst: &FileSummaryResource{
				Name:        "aws_instance.web",
				Line:        0,
				MonthlyCost: "$0.00",
				TagIssues:   3, // 2 missing + 1 invalid
			},
		},
		{
			name: "zero start line clamped to 0",
			results: map[string]*scanner.ScanResult{
				"proj": {
					Resources: []scanner.ResourceResult{
						{Name: "aws_instance.web", Filename: tfFile, StartLine: 0, EndLine: 4, MonthlyCost: mustRat("5.00")},
					},
				},
			},
			wantCount: 1,
			wantFirst: &FileSummaryResource{
				Name:        "aws_instance.web",
				Line:        0, // clamped, not -1
				MonthlyCost: "$5.00",
			},
		},
		{
			name: "nil cost formats as zero",
			results: map[string]*scanner.ScanResult{
				"proj": {
					Resources: []scanner.ResourceResult{
						{Name: "aws_instance.web", Filename: tfFile, StartLine: 1, EndLine: 4, MonthlyCost: nil},
					},
				},
			},
			wantCount: 1,
			wantFirst: &FileSummaryResource{
				Name:        "aws_instance.web",
				Line:        0,
				MonthlyCost: "$0.00",
			},
		},
		{
			name: "multiple projects merged",
			results: map[string]*scanner.ScanResult{
				"proj1": {
					Resources: []scanner.ResourceResult{
						{Name: "aws_instance.web", Filename: tfFile, StartLine: 1, EndLine: 4, MonthlyCost: mustRat("10.00")},
					},
				},
				"proj2": {
					Resources: []scanner.ResourceResult{
						{Name: "aws_s3_bucket.data", Filename: tfFile, StartLine: 5, EndLine: 8, MonthlyCost: mustRat("1.00")},
					},
				},
			},
			wantCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewServer(nil, nil, api.NewTokenSource(nil))

			if tt.scanning {
				s.mu.Lock()
				s.scanningProjects["test"] = struct{}{}
				s.mu.Unlock()
			}

			for k, v := range tt.results {
				s.projectResults[k] = v
			}

			params := mustMarshal(fileSummaryParams{URI: fileURI})
			got, err := s.HandleFileSummary(context.Background(), params)
			require.NoError(t, err)

			result, ok := got.(FileSummaryResult)
			require.True(t, ok)

			assert.Len(t, result.Resources, tt.wantCount)

			if tt.wantFirst != nil && len(result.Resources) > 0 {
				assert.Equal(t, tt.wantFirst.Name, result.Resources[0].Name, "name")
				assert.Equal(t, tt.wantFirst.Line, result.Resources[0].Line, "line")
				assert.Equal(t, tt.wantFirst.MonthlyCost, result.Resources[0].MonthlyCost, "monthlyCost")
				assert.Equal(t, tt.wantFirst.PolicyIssues, result.Resources[0].PolicyIssues, "policyIssues")
				assert.Equal(t, tt.wantFirst.TagIssues, result.Resources[0].TagIssues, "tagIssues")
			}
		})
	}
}

func TestHandleFileSummaryInvalidParams(t *testing.T) {
	s := NewServer(nil, nil, api.NewTokenSource(nil))
	_, err := s.HandleFileSummary(context.Background(), []byte(`{invalid`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid params")
}
