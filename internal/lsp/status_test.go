package lsp

import (
	"context"
	"encoding/json"
	"testing"

	repoconfig "github.com/infracost/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/infracost/lsp/internal/api"
	"github.com/infracost/lsp/internal/scanner"
	"github.com/infracost/lsp/version"
)

func TestHandleStatus(t *testing.T) {
	tests := []struct {
		name     string
		scanning bool
		config   *repoconfig.Config
		results  map[string]*scanner.ScanResult
		want     StatusResult
	}{
		{
			name: "bare server",
			want: StatusResult{
				Version: version.Version,
			},
		},
		{
			name:     "scanning",
			scanning: true,
			want: StatusResult{
				Version:  version.Version,
				Scanning: true,
			},
		},
		{
			name: "with config and projects",
			config: &repoconfig.Config{
				Projects: []*repoconfig.Project{
					{Name: "prod"},
					{Name: "staging"},
				},
			},
			want: StatusResult{
				Version:      version.Version,
				ConfigFound:  true,
				ProjectCount: 2,
				ProjectNames: []string{"prod", "staging"},
			},
		},
		{
			name: "with scan results",
			results: map[string]*scanner.ScanResult{
				"proj": {
					Resources: []scanner.ResourceResult{
						{Name: "aws_instance.web"},
						{Name: "aws_s3_bucket.data"},
					},
					Violations: []scanner.FinopsViolation{
						{PolicySlug: "use-graviton"},
					},
					TagViolations: []scanner.TagViolation{
						{Address: "aws_instance.web", MissingTags: []string{"env"}},
						{Address: "aws_s3_bucket.data", MissingTags: []string{"team"}},
					},
				},
			},
			want: StatusResult{
				Version:        version.Version,
				ResourceCount:  2,
				ViolationCount: 1,
				TagIssueCount:  2,
			},
		},
		{
			name: "tag issues counts individual tags not violations",
			results: map[string]*scanner.ScanResult{
				"proj": {
					TagViolations: []scanner.TagViolation{
						{
							Address:     "aws_instance.web",
							MissingTags: []string{"env", "team", "owner"},
							InvalidTags: []scanner.InvalidTagResult{
								{Key: "cost-center", Value: "bad"},
							},
						},
					},
				},
			},
			want: StatusResult{
				Version:       version.Version,
				TagIssueCount: 4, // 3 missing + 1 invalid
			},
		},
		{
			name: "config with no projects has empty slice",
			config: &repoconfig.Config{
				Projects: []*repoconfig.Project{},
			},
			want: StatusResult{
				Version:      version.Version,
				ConfigFound:  true,
				ProjectNames: []string{},
			},
		},
		{
			name:     "scanning with results",
			scanning: true,
			results: map[string]*scanner.ScanResult{
				"proj": {
					Resources: []scanner.ResourceResult{
						{Name: "aws_instance.web"},
					},
				},
			},
			want: StatusResult{
				Version:       version.Version,
				Scanning:      true,
				ResourceCount: 1,
			},
		},
		{
			name: "multiple projects merged",
			results: map[string]*scanner.ScanResult{
				"proj1": {
					Resources:  []scanner.ResourceResult{{Name: "r1"}},
					Violations: []scanner.FinopsViolation{{PolicySlug: "v1"}},
				},
				"proj2": {
					Resources:     []scanner.ResourceResult{{Name: "r2"}, {Name: "r3"}},
					TagViolations: []scanner.TagViolation{{Address: "r2", MissingTags: []string{"env"}}},
				},
			},
			want: StatusResult{
				Version:        version.Version,
				ResourceCount:  3,
				ViolationCount: 1,
				TagIssueCount:  1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewServer(&scanner.Scanner{}, nil, api.NewTokenSource(nil))

			if tt.scanning {
				s.mu.Lock()
				s.scanningProjects["test"] = struct{}{}
				s.mu.Unlock()
			}

			if tt.config != nil {
				s.setConfig(tt.config)
			}

			for k, v := range tt.results {
				s.projectResults[k] = v
			}

			got, err := s.HandleStatus(context.Background(), nil)
			require.NoError(t, err)

			result, ok := got.(StatusResult)
			require.True(t, ok)

			assert.Equal(t, tt.want.Version, result.Version, "version")
			assert.Equal(t, tt.want.Scanning, result.Scanning, "scanning")
			assert.Equal(t, tt.want.ConfigFound, result.ConfigFound, "configFound")
			assert.Equal(t, tt.want.ProjectCount, result.ProjectCount, "projectCount")
			assert.Equal(t, tt.want.ProjectNames, result.ProjectNames, "projectNames")
			assert.Equal(t, tt.want.ResourceCount, result.ResourceCount, "resourceCount")
			assert.Equal(t, tt.want.ViolationCount, result.ViolationCount, "violationCount")
			assert.Equal(t, tt.want.TagIssueCount, result.TagIssueCount, "tagIssueCount")
		})
	}
}

func TestHandleStatusNilScanner(t *testing.T) {
	s := NewServer(nil, nil, api.NewTokenSource(nil))

	got, err := s.HandleStatus(context.Background(), nil)
	require.NoError(t, err)

	result, ok := got.(StatusResult)
	require.True(t, ok)
	assert.False(t, result.LoggedIn, "loggedIn should be false with nil scanner")
}

func TestHandleStatusProjectNamesJSON(t *testing.T) {
	s := NewServer(&scanner.Scanner{}, nil, api.NewTokenSource(nil))
	s.setConfig(&repoconfig.Config{
		Projects: []*repoconfig.Project{},
	})

	got, err := s.HandleStatus(context.Background(), nil)
	require.NoError(t, err)

	b, err := json.Marshal(got)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"projectNames":[]`, "empty projects should serialize as [] not null")
}
