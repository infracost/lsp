package scanner

import (
	"testing"

	goprotoevent "github.com/infracost/go-proto/pkg/event"
	"github.com/infracost/proto/gen/go/infracost/provider"
)

func TestConvertTagViolations(t *testing.T) {
	resources := []*provider.Resource{
		{
			Name: "aws_instance.web",
			Type: "aws_instance",
			Metadata: &provider.ResourceMetadata{
				Filename:  "main.tf",
				StartLine: 1,
				EndLine:   10,
			},
		},
		{
			Name: "aws_s3_bucket.data",
			Type: "aws_s3_bucket",
		},
	}

	srcLocs := map[string]sourceLocation{
		"aws_s3_bucket.data": {
			Filename:  "storage.tf",
			StartLine: 5,
			EndLine:   15,
		},
	}

	tests := []struct {
		name    string
		results []goprotoevent.TaggingPolicyResult
		want    []TagViolation
	}{
		{
			name:    "empty results",
			results: nil,
			want:    nil,
		},
		{
			name: "missing mandatory tags",
			results: []goprotoevent.TaggingPolicyResult{
				{
					Name:    "Required Tags",
					BlockPR: true,
					FailingResources: []goprotoevent.TagPolicyResultResource{
						{
							Address:              "aws_instance.web",
							ResourceType:         "aws_instance",
							MissingMandatoryTags: []string{"env", "team"},
						},
					},
				},
			},
			want: []TagViolation{
				{
					PolicyName:   "Required Tags",
					BlockPR:      true,
					Address:      "aws_instance.web",
					ResourceType: "aws_instance",
					Filename:     "/project/main.tf",
					StartLine:    1,
					EndLine:      10,
					Message:      "Missing mandatory tags: env, team",
					MissingTags:  []string{"env", "team"},
				},
			},
		},
		{
			name: "invalid tags",
			results: []goprotoevent.TaggingPolicyResult{
				{
					Name: "Env Tag Policy",
					FailingResources: []goprotoevent.TagPolicyResultResource{
						{
							Address:      "aws_s3_bucket.data",
							ResourceType: "aws_s3_bucket",
							InvalidTags: []goprotoevent.InvalidTag{
								{
									Key:        "env",
									Value:      "test",
									Suggestion: "staging",
									Message:    "Invalid tag `env`: expected one of [prod, staging, dev]",
								},
							},
						},
					},
				},
			},
			want: []TagViolation{
				{
					PolicyName:   "Env Tag Policy",
					Address:      "aws_s3_bucket.data",
					ResourceType: "aws_s3_bucket",
					Filename:     "/project/storage.tf",
					StartLine:    5,
					EndLine:      15,
					Message:      "Invalid tag `env`: expected one of [prod, staging, dev]",
					InvalidTags: []InvalidTagResult{
						{
							Key:        "env",
							Value:      "test",
							Suggestion: "staging",
							Message:    "Invalid tag `env`: expected one of [prod, staging, dev]",
						},
					},
				},
			},
		},
		{
			name: "mixed missing and invalid",
			results: []goprotoevent.TaggingPolicyResult{
				{
					Name:    "All Tags",
					BlockPR: true,
					FailingResources: []goprotoevent.TagPolicyResultResource{
						{
							Address:              "aws_instance.web",
							ResourceType:         "aws_instance",
							MissingMandatoryTags: []string{"cost-center"},
							InvalidTags: []goprotoevent.InvalidTag{
								{Key: "env", Value: "bad", Message: "env must be prod or dev"},
							},
						},
					},
				},
			},
			want: []TagViolation{
				{
					PolicyName:   "All Tags",
					BlockPR:      true,
					Address:      "aws_instance.web",
					ResourceType: "aws_instance",
					Filename:     "/project/main.tf",
					StartLine:    1,
					EndLine:      10,
					Message:      "Missing mandatory tags: cost-center; env must be prod or dev",
					MissingTags:  []string{"cost-center"},
					InvalidTags: []InvalidTagResult{
						{Key: "env", Value: "bad", Message: "env must be prod or dev"},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertTagViolations(tt.results, resources, "/project", srcLocs)

			if len(got) != len(tt.want) {
				t.Fatalf("got %d violations, want %d", len(got), len(tt.want))
			}

			for i, g := range got {
				w := tt.want[i]
				if g.PolicyName != w.PolicyName {
					t.Errorf("[%d] PolicyName = %q, want %q", i, g.PolicyName, w.PolicyName)
				}
				if g.BlockPR != w.BlockPR {
					t.Errorf("[%d] BlockPR = %v, want %v", i, g.BlockPR, w.BlockPR)
				}
				if g.Address != w.Address {
					t.Errorf("[%d] Address = %q, want %q", i, g.Address, w.Address)
				}
				if g.ResourceType != w.ResourceType {
					t.Errorf("[%d] ResourceType = %q, want %q", i, g.ResourceType, w.ResourceType)
				}
				if g.Filename != w.Filename {
					t.Errorf("[%d] Filename = %q, want %q", i, g.Filename, w.Filename)
				}
				if g.StartLine != w.StartLine {
					t.Errorf("[%d] StartLine = %d, want %d", i, g.StartLine, w.StartLine)
				}
				if g.EndLine != w.EndLine {
					t.Errorf("[%d] EndLine = %d, want %d", i, g.EndLine, w.EndLine)
				}
				if g.Message != w.Message {
					t.Errorf("[%d] Message = %q, want %q", i, g.Message, w.Message)
				}
				if len(g.MissingTags) != len(w.MissingTags) {
					t.Errorf("[%d] MissingTags len = %d, want %d", i, len(g.MissingTags), len(w.MissingTags))
				}
				if len(g.InvalidTags) != len(w.InvalidTags) {
					t.Errorf("[%d] InvalidTags len = %d, want %d", i, len(g.InvalidTags), len(w.InvalidTags))
				}
				for j, it := range g.InvalidTags {
					if j >= len(w.InvalidTags) {
						break
					}
					wit := w.InvalidTags[j]
					if it.Key != wit.Key || it.Value != wit.Value || it.Suggestion != wit.Suggestion || it.Message != wit.Message {
						t.Errorf("[%d] InvalidTag[%d] = %+v, want %+v", i, j, it, wit)
					}
				}
			}
		})
	}
}
