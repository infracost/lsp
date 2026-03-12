package lsp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/owenrumney/go-lsp/lsp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/infracost/lsp/internal/scanner"
)

func TestInlayHint(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)
	testFile := filepath.Join(cwd, "testdata", "main.tf")
	testURI := "file://" + testFile

	tests := []struct {
		name       string
		resources  []scanner.ResourceResult
		modules    []scanner.ModuleCost
		violations []scanner.FinopsViolation
		tagViols   []scanner.TagViolation
		scanning   bool
		wantCount  int
		wantLabels []string
	}{
		{
			name:      "no results",
			wantCount: 0,
		},
		{
			name: "single resource cost",
			resources: []scanner.ResourceResult{
				{
					Name:        "aws_instance.web",
					Filename:    testFile,
					StartLine:   1,
					EndLine:     5,
					MonthlyCost: mustRat("25.00"),
					IsSupported: true,
				},
			},
			wantCount:  1,
			wantLabels: []string{"$25.00/mo"},
		},
		{
			name: "unsupported resource",
			resources: []scanner.ResourceResult{
				{
					Name:        "aws_iam_role.test",
					Filename:    testFile,
					StartLine:   1,
					EndLine:     5,
					IsSupported: false,
				},
			},
			wantCount:  1,
			wantLabels: []string{"Not supported"},
		},
		{
			name:     "scanning shows calculating",
			scanning: true,
			resources: []scanner.ResourceResult{
				{
					Name:        "aws_instance.web",
					Filename:    testFile,
					StartLine:   1,
					EndLine:     5,
					MonthlyCost: mustRat("25.00"),
					IsSupported: true,
				},
			},
			wantCount:  1,
			wantLabels: []string{"Calculating..."},
		},
		{
			name: "resource with finops violations",
			resources: []scanner.ResourceResult{
				{
					Name:        "aws_instance.web",
					Filename:    testFile,
					StartLine:   1,
					EndLine:     5,
					MonthlyCost: mustRat("25.00"),
					IsSupported: true,
				},
			},
			violations: []scanner.FinopsViolation{
				{Address: "aws_instance.web", PolicyName: "Use gp3", Message: "Use gp3"},
				{Address: "aws_instance.web", PolicyName: "Use ARM", Message: "Use ARM"},
			},
			wantCount:  2, // cost + finops
			wantLabels: []string{"$25.00/mo", "2 FinOps issues"},
		},
		{
			name: "resource with tag violations",
			resources: []scanner.ResourceResult{
				{
					Name:        "aws_instance.web",
					Filename:    testFile,
					StartLine:   1,
					EndLine:     5,
					MonthlyCost: mustRat("10.00"),
					IsSupported: true,
				},
			},
			tagViols: []scanner.TagViolation{
				{Address: "aws_instance.web", PolicyName: "Required tags"},
			},
			wantCount:  2, // cost + tag
			wantLabels: []string{"$10.00/mo", "1 tag issue"},
		},
		{
			name: "module cost",
			modules: []scanner.ModuleCost{
				{
					Name:          "module.vpc",
					Filename:      testFile,
					StartLine:     1,
					EndLine:       5,
					MonthlyCost:   mustRat("100.00"),
					ResourceCount: 3,
				},
			},
			wantCount:  1,
			wantLabels: []string{"$100.00/mo (3 resources)"},
		},
		{
			name: "resource in different file ignored",
			resources: []scanner.ResourceResult{
				{
					Name:        "aws_instance.other",
					Filename:    "/some/other/file.tf",
					StartLine:   1,
					EndLine:     5,
					MonthlyCost: mustRat("25.00"),
					IsSupported: true,
				},
			},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewServer(nil)

			if tt.scanning {
				srv.scanningProjects["test"] = struct{}{}
			}

			result := &scanner.ScanResult{
				Resources:     tt.resources,
				ModuleCosts:   tt.modules,
				Violations:    tt.violations,
				TagViolations: tt.tagViols,
			}
			srv.projectResults["test"] = result

			hints, err := srv.InlayHint(context.Background(), &lsp.InlayHintParams{
				TextDocument: lsp.TextDocumentIdentifier{URI: lsp.DocumentURI(testURI)},
				Range: lsp.Range{
					Start: lsp.Position{Line: 0, Character: 0},
					End:   lsp.Position{Line: 100, Character: 0},
				},
			})
			require.NoError(t, err)
			require.Len(t, hints, tt.wantCount)

			for i, wantLabel := range tt.wantLabels {
				if i >= len(hints) {
					break
				}
				gotLabel := string(hints[i].Label)
				// Label is JSON-encoded string, so it includes quotes.
				wantJSON := string(marshalLabel(wantLabel))
				assert.Equal(t, wantJSON, gotLabel, "hint[%d] label", i)
			}

			// All hints should have paddingLeft=true and position at character 999.
			for i, h := range hints {
				if assert.NotNil(t, h.PaddingLeft, "hint[%d] paddingLeft", i) {
					assert.True(t, *h.PaddingLeft, "hint[%d] paddingLeft should be true", i)
				}
				assert.Equal(t, 999, h.Position.Character, "hint[%d] character", i)
			}
		})
	}
}
