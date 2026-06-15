package lsp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRemoteModulePath(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "github blob url",
			url:  "https://github.com/RaJiska/terraform-aws-fck-nat/blob/1.4.0/ec2.tf#L58-L118",
			want: "Remote modules/RaJiska/terraform-aws-fck-nat@1.4.0/ec2.tf",
		},
		{
			name: "github tree url",
			url:  "https://github.com/org/repo/tree/v2.0.0/main.tf",
			want: "Remote modules/org/repo@v2.0.0/main.tf",
		},
		{
			name: "git source with ref query",
			url:  "git::https://github.com/org/repo//modules/vpc?ref=v1.2.3",
			want: "Remote modules/org/repo@v1.2.3/vpc",
		},
		{
			name: "no recognisable ref",
			url:  "https://example.com/some/path/network.tf",
			want: "Remote modules/some/path/network.tf",
		},
		{
			name: "owner/repo only does not repeat repo as file",
			url:  "https://github.com/org/repo",
			want: "Remote modules/org/repo",
		},
		{
			name: "single path segment",
			url:  "https://example.com/standalone",
			want: "Remote modules/standalone",
		},
		{
			name: "unparseable falls back to group",
			url:  "https://",
			want: "Remote modules",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := remoteModulePath(tt.url)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestContainsResource(t *testing.T) {
	rs := []WorkspaceSummaryResource{
		{Name: "aws_instance.a", Line: 10},
		{Name: "aws_instance.b", Line: 20},
	}

	assert.True(t, containsResource(rs, WorkspaceSummaryResource{Name: "aws_instance.a", Line: 10}))
	assert.False(t, containsResource(rs, WorkspaceSummaryResource{Name: "aws_instance.a", Line: 11}))
	assert.False(t, containsResource(rs, WorkspaceSummaryResource{Name: "aws_instance.c", Line: 10}))
}
