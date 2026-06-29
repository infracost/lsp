package lsp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/infracost/lsp/internal/scanner"
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

func TestHandleWorkspaceSummaryHidesRemoteModulesByDefault(t *testing.T) {
	root := t.TempDir()
	localFile := filepath.Join(root, "main.tf")
	moduleFile := filepath.Join(root, "modules", "infracost_aws", "main.tf")
	remoteFile := "https://github.com/org/repo/blob/v1.0.0/main.tf#L10-L20"

	srv := NewServer(nil, nil, nil)
	srv.workspaceRoot = root
	srv.projectResults["test"] = &scanner.ScanResult{
		Resources: []scanner.ResourceResult{
			{Name: "aws_instance.local", Filename: localFile, StartLine: 3},
			{
				Name:      "module.infracost_aws.module.eks.aws_instance.remote",
				Filename:  remoteFile,
				StartLine: 10,
				ModuleCallStack: []scanner.ModuleCall{
					{Name: "module.infracost_aws", Filename: localFile, StartLine: 5},
					{Name: "module.infracost_aws.module.eks", Filename: moduleFile, StartLine: 12},
				},
			},
		},
		Violations: []scanner.FinopsViolation{
			{Address: "aws_instance.local", Filename: localFile, PolicySlug: "policy"},
			{Address: "module.infracost_aws.module.eks.aws_instance.remote", Filename: remoteFile, PolicySlug: "policy"},
		},
	}

	got, err := srv.HandleWorkspaceSummary(context.Background(), nil)
	require.NoError(t, err)

	result := got.(WorkspaceSummaryResult)
	require.Len(t, result.Files, 1)
	assert.Equal(t, "main.tf", result.Files[0].Path)
	assert.True(t, result.Files[0].Openable)
	require.Len(t, result.Tree, 1)
	assert.Equal(t, "main.tf", result.Tree[0].Label)
	require.Len(t, result.Tree[0].Children, 1)
	assert.Equal(t, "aws_instance.local", result.Tree[0].Children[0].Label)
}

func TestHandleWorkspaceSummaryIncludesRemoteModulesAsNonOpenable(t *testing.T) {
	root := t.TempDir()
	localFile := filepath.Join(root, "dev", "main.tf")
	moduleFile := filepath.Join(root, "modules", "infracost_aws", "main.tf")
	remoteFile := "https://github.com/org/repo/blob/v1.0.0/main.tf#L10-L20"

	srv := NewServer(nil, nil, nil)
	srv.workspaceRoot = root
	srv.settings.DisplayRemoteModulesInTree = true
	srv.projectResults["test"] = &scanner.ScanResult{
		Resources: []scanner.ResourceResult{
			{Name: "aws_instance.local", Filename: localFile, StartLine: 3},
			{
				Name:      "module.infracost_aws.module.eks.aws_instance.remote",
				Filename:  remoteFile,
				StartLine: 10,
				ModuleCallStack: []scanner.ModuleCall{
					{Name: "module.infracost_aws", Filename: localFile, StartLine: 5},
					{Name: "module.infracost_aws.module.eks", Filename: moduleFile, StartLine: 12},
				},
			},
		},
		Violations: []scanner.FinopsViolation{
			{Address: "aws_instance.local", Filename: localFile, PolicySlug: "policy"},
			{Address: "module.infracost_aws.module.eks.aws_instance.remote", Filename: remoteFile, PolicySlug: "policy"},
		},
	}

	got, err := srv.HandleWorkspaceSummary(context.Background(), nil)
	require.NoError(t, err)

	result := got.(WorkspaceSummaryResult)
	require.Len(t, result.Files, 2)
	assert.Equal(t, "Remote modules/org/repo@v1.0.0/main.tf", result.Files[0].Path)
	assert.False(t, result.Files[0].Openable)
	assert.Equal(t, "dev/main.tf", result.Files[1].Path)
	assert.True(t, result.Files[1].Openable)

	require.Len(t, result.Tree, 1)
	devFolder := result.Tree[0]
	assert.Equal(t, "dev", devFolder.Label)
	require.Len(t, devFolder.Children, 1)
	rootFile := devFolder.Children[0]
	assert.Equal(t, "main.tf", rootFile.Label)
	assert.Equal(t, "dev/main.tf", rootFile.Path)
	require.Len(t, rootFile.Children, 2)
	assert.Equal(t, "module.infracost_aws", rootFile.Children[0].Label)
	assert.True(t, rootFile.Children[0].Openable)

	localModuleFile := rootFile.Children[0].Children[0]
	assert.Equal(t, "main.tf", localModuleFile.Label)
	assert.Equal(t, "modules/infracost_aws/main.tf", localModuleFile.Path)
	require.Len(t, localModuleFile.Children, 1)
	assert.Equal(t, "module.eks", localModuleFile.Children[0].Label)
	assert.True(t, localModuleFile.Children[0].Openable)

	remoteModuleFile := localModuleFile.Children[0].Children[0]
	assert.Equal(t, "main.tf", remoteModuleFile.Label)
	assert.Equal(t, "Remote modules/org/repo@v1.0.0/main.tf", remoteModuleFile.Path)
	assert.False(t, remoteModuleFile.Openable)
	require.Len(t, remoteModuleFile.Children, 1)
	assert.Equal(t, "aws_instance.remote", remoteModuleFile.Children[0].Label)
	assert.Equal(t, "module.infracost_aws.module.eks.aws_instance.remote", remoteModuleFile.Children[0].Path)
	assert.False(t, remoteModuleFile.Children[0].Openable)
}

func TestHandleWorkspaceSummarySkipsDuplicateCurrentModuleFrame(t *testing.T) {
	root := t.TempDir()
	rootFile := filepath.Join(root, "dev", "carbon.tf")
	moduleFile := filepath.Join(root, "modules", "greenpixie_partner", "main.tf")

	srv := NewServer(nil, nil, nil)
	srv.workspaceRoot = root
	srv.projectResults["test"] = &scanner.ScanResult{
		Resources: []scanner.ResourceResult{
			{
				Name:     "module.greenpixie_partner.aws_iam_role.greenpixie_partner",
				Filename: moduleFile,
				ModuleCallStack: []scanner.ModuleCall{
					{Name: "module.greenpixie_partner", Filename: rootFile, StartLine: 5},
					{Name: "module.greenpixie_partner", Filename: moduleFile, StartLine: 1},
				},
			},
		},
		Violations: []scanner.FinopsViolation{
			{Address: "module.greenpixie_partner.aws_iam_role.greenpixie_partner", Filename: moduleFile, PolicySlug: "policy"},
		},
	}

	got, err := srv.HandleWorkspaceSummary(context.Background(), nil)
	require.NoError(t, err)

	result := got.(WorkspaceSummaryResult)
	mainTF := result.Tree[0].Children[0].Children[0].Children[0]
	assert.Equal(t, "main.tf", mainTF.Label)
	require.Len(t, mainTF.Children, 1)
	assert.Equal(t, "resource", mainTF.Children[0].Type)
	assert.Equal(t, "aws_iam_role.greenpixie_partner", mainTF.Children[0].Label)
}

func TestWorkspaceSummaryResourceLabel(t *testing.T) {
	assert.Equal(t, "aws_instance.root", workspaceSummaryResourceLabel("aws_instance.root", nil))
	assert.Equal(t,
		"aws_iam_role.greenpixie_partner",
		workspaceSummaryResourceLabel(
			"module.greenpixie_partner.aws_iam_role.greenpixie_partner",
			[]scanner.ModuleCall{{Name: "module.greenpixie_partner"}},
		),
	)
	assert.Equal(t,
		"aws_instance.remote",
		workspaceSummaryResourceLabel(
			"module.infracost_aws.module.eks.aws_instance.remote",
			[]scanner.ModuleCall{{Name: "module.infracost_aws"}, {Name: "module.infracost_aws.module.eks"}},
		),
	)
	assert.Equal(t,
		"aws_backup_plan.database_backups",
		workspaceSummaryResourceLabel(
			"module.base.aws_backup_plan.database_backups",
			[]scanner.ModuleCall{{Name: "module.base"}, {Name: "module.base.module.database_backups"}},
		),
	)
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
