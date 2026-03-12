package lsp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUriToPath(t *testing.T) {
	tests := []struct {
		name string
		uri  string
		want string
	}{
		// Standard file:// URIs — Unix paths
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

		// Windows file:// URIs — drive letter handling
		{
			name: "windows drive letter lowercase",
			uri:  "file:///c:/Users/owen/project/main.tf",
			want: "c:/Users/owen/project/main.tf",
		},
		{
			name: "windows drive letter uppercase",
			uri:  "file:///C:/Users/owen/project/main.tf",
			want: "C:/Users/owen/project/main.tf",
		},
		{
			name: "windows drive letter with percent-encoded colon",
			uri:  "file:///c%3A/Users/owen/project/main.tf",
			want: "c:/Users/owen/project/main.tf",
		},
		{
			name: "windows drive letter uppercase with percent-encoded colon",
			uri:  "file:///C%3A/Users/owen/project/main.tf",
			want: "C:/Users/owen/project/main.tf",
		},
		{
			name: "windows path with spaces (percent-encoded)",
			uri:  "file:///c%3A/Users/owen/example%20terraform/main.tf",
			want: "c:/Users/owen/example terraform/main.tf",
		},
		{
			name: "windows path with spaces in drive-letter URI",
			uri:  "file:///c:/Users/owen/example terraform/main.tf",
			want: "c:/Users/owen/example terraform/main.tf",
		},

		// Non-file:// inputs — POSIX-style Windows paths without scheme
		{
			name: "posix-style windows path without file scheme",
			uri:  "/c:/Users/owen/project",
			want: "c:/Users/owen/project",
		},
		{
			name: "posix-style windows path uppercase drive",
			uri:  "/C:/Users/owen/project",
			want: "C:/Users/owen/project",
		},

		// Non-file:// inputs — plain paths passed through unchanged
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
