package lsp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/owenrumney/go-lsp/lsp"
	"github.com/owenrumney/go-lsp/servertest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/infracost/lsp/internal/api"
)

func TestInitializeCapabilities(t *testing.T) {
	h := servertest.New(t, NewServer(nil, nil, api.NewTokenSource(nil)))

	result := h.InitResult
	require.NotNil(t, result)

	// Text document sync
	require.NotNil(t, result.Capabilities.TextDocumentSync)
	assert.Equal(t, lsp.SyncIncremental, result.Capabilities.TextDocumentSync.Change)

	// Code lens
	assert.NotNil(t, result.Capabilities.CodeLensProvider)

	// Code action with quick fix
	require.NotNil(t, result.Capabilities.CodeActionProvider)
	assert.Contains(t, result.Capabilities.CodeActionProvider.CodeActionKinds, lsp.CodeActionQuickFix)

	// Hover
	require.NotNil(t, result.Capabilities.HoverProvider)
	assert.True(t, *result.Capabilities.HoverProvider)

	// Workspace folders
	require.NotNil(t, result.Capabilities.Workspace)
	require.NotNil(t, result.Capabilities.Workspace.WorkspaceFolders)
	assert.True(t, *result.Capabilities.Workspace.WorkspaceFolders.Supported)

	// Execute command
	require.NotNil(t, result.Capabilities.ExecuteCommandProvider)
	assert.Contains(t, result.Capabilities.ExecuteCommandProvider.Commands, "infracost.dismissDiagnostic")

	// Server info
	require.NotNil(t, result.ServerInfo)
	assert.Equal(t, "infracost-ls", result.ServerInfo.Name)
}

func TestUriToPath_Windows(t *testing.T) {
	orig := isWindows
	isWindows = true
	t.Cleanup(func() { isWindows = orig })

	tests := []struct {
		name string
		uri  string
		want string
	}{
		// file:// URIs with drive letters
		{
			name: "drive letter lowercase",
			uri:  "file:///c:/Users/owen/project/main.tf",
			want: `C:\Users\owen\project\main.tf`,
		},
		{
			name: "drive letter uppercase",
			uri:  "file:///C:/Users/owen/project/main.tf",
			want: `C:\Users\owen\project\main.tf`,
		},
		{
			name: "drive letter with percent-encoded colon",
			uri:  "file:///c%3A/Users/owen/project/main.tf",
			want: `C:\Users\owen\project\main.tf`,
		},
		{
			name: "drive letter uppercase with percent-encoded colon",
			uri:  "file:///C%3A/Users/owen/project/main.tf",
			want: `C:\Users\owen\project\main.tf`,
		},
		{
			name: "path with spaces (percent-encoded)",
			uri:  "file:///c%3A/Users/owen/example%20terraform/main.tf",
			want: `C:\Users\owen\example terraform\main.tf`,
		},
		{
			name: "path with spaces in drive-letter URI",
			uri:  "file:///c:/Users/owen/example terraform/main.tf",
			want: `C:\Users\owen\example terraform\main.tf`,
		},

		// POSIX-style Windows paths without file:// prefix
		{
			name: "posix-style path without file scheme",
			uri:  "/c:/Users/owen/project",
			want: `c:\Users\owen\project`,
		},
		{
			name: "posix-style path uppercase drive",
			uri:  "/C:/Users/owen/project",
			want: `C:\Users\owen\project`,
		},

		// Passthrough cases that should still work on Windows
		{
			name: "empty string",
			uri:  "",
			want: "",
		},
		{
			name: "relative path passthrough",
			uri:  "project/main.tf",
			want: "project/main.tf",
		},
		{
			name: "non-file scheme passthrough",
			uri:  "untitled:///untitled-1",
			want: "untitled:///untitled-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := uriToPath(tt.uri)
			assert.Equal(t, tt.want, got, "uriToPath(%q)", tt.uri)
		})
	}
}

func TestUriToPath_Unix(t *testing.T) {
	orig := isWindows
	isWindows = false
	t.Cleanup(func() { isWindows = orig })

	tests := []struct {
		name string
		uri  string
		want string
	}{
		{
			name: "unix absolute path",
			uri:  "file:///home/user/project/main.tf",
			want: "/home/user/project/main.tf",
		},
		{
			name: "unix path with spaces (percent-encoded)",
			uri:  "file:///home/user/my%20project/main.tf",
			want: "/home/user/my project/main.tf",
		},
		{
			name: "drive-letter URI left intact on unix",
			uri:  "file:///c:/Users/owen/project/main.tf",
			want: "/c:/Users/owen/project/main.tf",
		},
		{
			name: "posix-style drive path left intact on unix",
			uri:  "/c:/Users/owen/project",
			want: "/c:/Users/owen/project",
		},
		{
			name: "plain unix path passthrough",
			uri:  "/home/user/project",
			want: "/home/user/project",
		},
		{
			name: "empty string",
			uri:  "",
			want: "",
		},
		{
			name: "relative path passthrough",
			uri:  "project/main.tf",
			want: "project/main.tf",
		},
		{
			name: "non-file scheme passthrough",
			uri:  "untitled:///untitled-1",
			want: "untitled:///untitled-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := uriToPath(tt.uri)
			assert.Equal(t, tt.want, got, "uriToPath(%q)", tt.uri)
		})
	}
}

func TestIsSupportedFile(t *testing.T) {
	tests := []struct {
		name string
		uri  string
		want bool
	}{
		{"terraform file", "file:///project/main.tf", true},
		{"terragrunt file", "file:///project/terragrunt.hcl", true},
		{"go file", "file:///project/main.go", false},
		{"plain text", "file:///project/notes.txt", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isSupportedFile(tt.uri), "isSupportedFile(%q)", tt.uri)
		})
	}
}

func TestIsSupportedFile_CloudFormation(t *testing.T) {
	tests := []struct {
		name    string
		ext     string
		content string
		want    bool
	}{
		{
			name:    "yaml with Resources",
			ext:     ".yaml",
			content: "AWSTemplateFormatVersion: '2010-09-09'\nResources:\n  MyBucket:\n    Type: AWS::S3::Bucket\n",
			want:    true,
		},
		{
			name:    "yml with Resources",
			ext:     ".yml",
			content: "Resources:\n  MyFunc:\n    Type: AWS::Lambda::Function\n",
			want:    true,
		},
		{
			name:    "json with Resources",
			ext:     ".json",
			content: `{"AWSTemplateFormatVersion": "2010-09-09", "Resources": {}}`,
			want:    true,
		},
		{
			name:    "yaml without cfn markers",
			ext:     ".yaml",
			content: "services:\n  web:\n    image: nginx\n",
			want:    false,
		},
		{
			name:    "json without cfn markers",
			ext:     ".json",
			content: `{"name": "my-package", "version": "1.0.0"}`,
			want:    false,
		},
		{
			name:    "empty yaml",
			ext:     ".yaml",
			content: "",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "template"+tt.ext)
			err := os.WriteFile(f, []byte(tt.content), 0o644)
			require.NoError(t, err)

			uri := "file://" + f
			assert.Equal(t, tt.want, isSupportedFile(uri), "isSupportedFile(%q)", tt.name)
		})
	}
}
