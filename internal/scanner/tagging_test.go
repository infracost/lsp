package scanner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

			require.Len(t, got, len(tt.want))

			for i, g := range got {
				w := tt.want[i]
				assert.Equal(t, w.PolicyName, g.PolicyName, "[%d] PolicyName", i)
				assert.Equal(t, w.BlockPR, g.BlockPR, "[%d] BlockPR", i)
				assert.Equal(t, w.Address, g.Address, "[%d] Address", i)
				assert.Equal(t, w.ResourceType, g.ResourceType, "[%d] ResourceType", i)
				assert.Equal(t, w.Filename, g.Filename, "[%d] Filename", i)
				assert.Equal(t, w.StartLine, g.StartLine, "[%d] StartLine", i)
				assert.Equal(t, w.EndLine, g.EndLine, "[%d] EndLine", i)
				assert.Equal(t, w.Message, g.Message, "[%d] Message", i)
				assert.Equal(t, w.MissingTags, g.MissingTags, "[%d] MissingTags", i)
				assert.Equal(t, w.InvalidTags, g.InvalidTags, "[%d] InvalidTags", i)
			}
		})
	}
}
