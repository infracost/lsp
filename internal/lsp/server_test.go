package lsp

import (
	"os"
	"path/filepath"
	"testing"
)

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
			if got := isSupportedFile(tt.uri); got != tt.want {
				t.Errorf("isSupportedFile(%q) = %v, want %v", tt.uri, got, tt.want)
			}
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
			if err := os.WriteFile(f, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}

			uri := "file://" + f
			if got := isSupportedFile(uri); got != tt.want {
				t.Errorf("isSupportedFile(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
