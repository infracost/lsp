package lsp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/owenrumney/go-lsp/lsp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/infracost/lsp/internal/scanner"
)

func TestCodeLensDedupesRepeatedResourceLensesOnModuleCallLine(t *testing.T) {
	tfFile := filepath.Join(t.TempDir(), "main.tf")
	uri := "file://" + tfFile

	srv := NewServer(nil, nil, nil)
	srv.projectResults["test"] = &scanner.ScanResult{
		Resources: []scanner.ResourceResult{
			{Name: "module.foo.aws_s3_bucket.one", Filename: tfFile, StartLine: 12, EndLine: 12, MonthlyCost: mustRat("0.10"), IsSupported: true},
			{Name: "module.foo.aws_s3_bucket.two", Filename: tfFile, StartLine: 12, EndLine: 12, MonthlyCost: mustRat("0.10"), IsSupported: true},
		},
		TagViolations: []scanner.TagViolation{
			{Address: "module.foo.aws_s3_bucket.one"},
			{Address: "module.foo.aws_s3_bucket.one"},
			{Address: "module.foo.aws_s3_bucket.two"},
			{Address: "module.foo.aws_s3_bucket.two"},
		},
	}

	lenses, err := srv.CodeLens(context.Background(), &lsp.CodeLensParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: lsp.DocumentURI(uri)},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"$0.10/mo", "2 tag issues"}, codeLensTitles(lenses))
}

func TestCodeLensPrefersModuleCostOverResourceCostsOnSameLine(t *testing.T) {
	tfFile := filepath.Join(t.TempDir(), "main.tf")
	uri := "file://" + tfFile

	srv := NewServer(nil, nil, nil)
	srv.projectResults["test"] = &scanner.ScanResult{
		Resources: []scanner.ResourceResult{
			{Name: "module.foo.aws_s3_bucket.one", Filename: tfFile, StartLine: 12, EndLine: 12, MonthlyCost: mustRat("0.10"), IsSupported: true},
			{Name: "module.foo.aws_s3_bucket.two", Filename: tfFile, StartLine: 12, EndLine: 12, MonthlyCost: mustRat("0.20"), IsSupported: true},
		},
		ModuleCosts: []scanner.ModuleCost{
			{Name: "module.foo", Filename: tfFile, StartLine: 12, EndLine: 12, MonthlyCost: mustRat("0.30"), ResourceCount: 2},
		},
	}

	lenses, err := srv.CodeLens(context.Background(), &lsp.CodeLensParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: lsp.DocumentURI(uri)},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"$0.30/mo (2 resources)"}, codeLensTitles(lenses))
}

func TestCodeLensPrefersHigherResourceCostOverLowerModuleCostOnSameLine(t *testing.T) {
	tfFile := filepath.Join(t.TempDir(), "main.tf")
	uri := "file://" + tfFile

	srv := NewServer(nil, nil, nil)
	srv.projectResults["test"] = &scanner.ScanResult{
		Resources: []scanner.ResourceResult{
			{Name: "module.foo.aws_instance.big", Filename: tfFile, StartLine: 12, EndLine: 12, MonthlyCost: mustRat("197.02"), IsSupported: true},
		},
		ModuleCosts: []scanner.ModuleCost{
			{Name: "module.foo", Filename: tfFile, StartLine: 12, EndLine: 12, MonthlyCost: mustRat("13.06"), ResourceCount: 1},
		},
	}

	lenses, err := srv.CodeLens(context.Background(), &lsp.CodeLensParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: lsp.DocumentURI(uri)},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"$197.02/mo"}, codeLensTitles(lenses))
}

func codeLensTitles(lenses []lsp.CodeLens) []string {
	titles := make([]string, 0, len(lenses))
	for _, lens := range lenses {
		if lens.Command != nil {
			titles = append(titles, lens.Command.Title)
		}
	}
	return titles
}

func TestPluralize(t *testing.T) {
	tests := []struct {
		name     string
		n        int
		singular string
		plural   string
		want     string
	}{
		{"zero uses plural", 0, "resource", "resources", "resources"},
		{"one uses singular", 1, "resource", "resources", "resource"},
		{"two uses plural", 2, "resource", "resources", "resources"},
		{"negative uses plural", -1, "resource", "resources", "resources"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pluralize(tt.n, tt.singular, tt.plural)
			assert.Equal(t, tt.want, got)
		})
	}
}
