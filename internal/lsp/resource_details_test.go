package lsp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/infracost/lsp/internal/api"
	"github.com/infracost/lsp/internal/scanner"
)

func TestHandleResourceDetailsUsesExactAddress(t *testing.T) {
	tfFile := filepath.Join(t.TempDir(), "main.tf")
	uri := "file://" + tfFile

	srv := NewServer(&scanner.Scanner{}, nil, api.NewTokenSource(nil))
	srv.projectResults["test"] = &scanner.ScanResult{
		Resources: []scanner.ResourceResult{
			{Name: "module.foo.aws_instance.small", Type: "aws_instance", Filename: tfFile, StartLine: 12, EndLine: 12, MonthlyCost: mustRat("13.06"), IsSupported: true},
			{Name: "module.foo.aws_instance.big", Type: "aws_instance", Filename: tfFile, StartLine: 12, EndLine: 12, MonthlyCost: mustRat("197.02"), IsSupported: true},
		},
	}

	params, err := json.Marshal(resourceDetailsParams{URI: uri, Line: 11, Address: "module.foo.aws_instance.big"})
	require.NoError(t, err)

	got, err := srv.HandleResourceDetails(context.Background(), params)
	require.NoError(t, err)

	result := got.(ResourceDetailsResult)
	require.NotNil(t, result.Resource)
	assert.Equal(t, "module.foo.aws_instance.big", result.Resource.Name)
	assert.Equal(t, "$197.02", result.Resource.MonthlyCost)
}

func TestHandleResourceDetailsLineFallbackPrefersHighestCost(t *testing.T) {
	tfFile := filepath.Join(t.TempDir(), "main.tf")
	uri := "file://" + tfFile

	srv := NewServer(&scanner.Scanner{}, nil, api.NewTokenSource(nil))
	srv.projectResults["test"] = &scanner.ScanResult{
		Resources: []scanner.ResourceResult{
			{Name: "module.foo.aws_instance.small", Type: "aws_instance", Filename: tfFile, StartLine: 12, EndLine: 12, MonthlyCost: mustRat("13.06"), IsSupported: true},
			{Name: "module.foo.aws_instance.big", Type: "aws_instance", Filename: tfFile, StartLine: 12, EndLine: 12, MonthlyCost: mustRat("197.02"), IsSupported: true},
		},
	}

	params, err := json.Marshal(resourceDetailsParams{URI: uri, Line: 11})
	require.NoError(t, err)

	got, err := srv.HandleResourceDetails(context.Background(), params)
	require.NoError(t, err)

	result := got.(ResourceDetailsResult)
	require.NotNil(t, result.Resource)
	assert.Equal(t, "module.foo.aws_instance.big", result.Resource.Name)
	assert.Equal(t, "$197.02", result.Resource.MonthlyCost)
}
