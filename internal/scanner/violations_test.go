package scanner

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/infracost/go-proto/pkg/rat"
	"github.com/infracost/lsp/internal/dashboard"
)

func TestBuildTagViolationMarkdown(t *testing.T) {
	tests := []struct {
		name     string
		v        TagViolation
		contains []string
	}{
		{
			name: "missing tags only",
			v: TagViolation{
				PolicyName:   "Require env tag",
				Address:      "aws_instance.web",
				ResourceType: "aws_instance",
				BlockPR:      false,
				MissingTags:  []string{"env", "team"},
			},
			contains: []string{
				"**Require env tag**",
				"`aws_instance.web`",
				"`aws_instance`",
				"**Severity:** Warning",
				"**Missing Mandatory Tags**",
				"- `env`",
				"- `team`",
			},
		},
		{
			name: "blocking severity",
			v: TagViolation{
				PolicyName:   "Cost center",
				Address:      "aws_s3_bucket.data",
				ResourceType: "aws_s3_bucket",
				BlockPR:      true,
				MissingTags:  []string{"cost_center"},
			},
			contains: []string{
				"**Severity:** Error (Blocking)",
			},
		},
		{
			name: "invalid tags with suggestion and message",
			v: TagViolation{
				PolicyName:   "Valid environments",
				Address:      "aws_instance.web",
				ResourceType: "aws_instance",
				InvalidTags: []InvalidTagResult{
					{Key: "env", Value: "prod-2", Suggestion: "Use 'production'", Message: "Invalid environment value"},
				},
			},
			contains: []string{
				"**Invalid Tags**",
				"- **env**: `prod-2`",
				"*Suggestion:* Use 'production'",
				"*Note:* Invalid environment value",
			},
		},
		{
			name: "invalid tag without suggestion or message",
			v: TagViolation{
				PolicyName:   "Valid teams",
				Address:      "aws_instance.web",
				ResourceType: "aws_instance",
				InvalidTags: []InvalidTagResult{
					{Key: "team", Value: "unknown"},
				},
			},
			contains: []string{
				"- **team**: `unknown`",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildTagViolationMarkdown(tt.v)
			for _, s := range tt.contains {
				assert.Contains(t, got, s)
			}
		})
	}
}

func TestBuildTagViolationMessage(t *testing.T) {
	tests := []struct {
		name string
		v    TagViolation
		want string
	}{
		{
			name: "missing tags",
			v: TagViolation{
				PolicyName:  "Tag policy",
				MissingTags: []string{"env", "team"},
			},
			want: "Missing mandatory tags: env, team",
		},
		{
			name: "invalid tag with message",
			v: TagViolation{
				PolicyName: "Tag policy",
				InvalidTags: []InvalidTagResult{
					{Key: "env", Value: "bad", Message: "Must be prod or staging"},
				},
			},
			want: "Must be prod or staging",
		},
		{
			name: "invalid tag without message",
			v: TagViolation{
				PolicyName: "Tag policy",
				InvalidTags: []InvalidTagResult{
					{Key: "env", Value: "bad"},
				},
			},
			want: "Invalid tag `env`: value `bad`",
		},
		{
			name: "no tags at all falls back to policy name",
			v: TagViolation{
				PolicyName: "Require tags",
			},
			want: "Tag policy violation: Require tags",
		},
		{
			name: "missing and invalid combined",
			v: TagViolation{
				PolicyName:  "Tag policy",
				MissingTags: []string{"owner"},
				InvalidTags: []InvalidTagResult{
					{Key: "env", Value: "x", Message: "Bad env"},
				},
			},
			want: "Missing mandatory tags: owner; Bad env",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildTagViolationMessage(tt.v)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBuildViolationMarkdown(t *testing.T) {
	tests := []struct {
		name     string
		v        FinopsViolation
		contains []string
		excludes []string
	}{
		{
			name: "basic violation",
			v: FinopsViolation{
				PolicyName: "Right-size instances",
				PolicySlug: "right-size",
				Address:    "aws_instance.web",
				Message:    "Instance is oversized",
			},
			contains: []string{
				"**Right-size instances**",
				"**Policy:** right-size",
				"`aws_instance.web`",
				"Instance is oversized",
				"**Severity:** Warning",
			},
			excludes: []string{"Attribute", "Potential Savings"},
		},
		{
			name: "blocking with attribute",
			v: FinopsViolation{
				PolicyName:       "No public IPs",
				PolicySlug:       "no-public-ip",
				Address:          "aws_instance.web",
				Attribute:        "associate_public_ip_address",
				BlockPullRequest: true,
				Message:          "Public IP not allowed",
			},
			contains: []string{
				"**Severity:** Error (Blocking)",
				"`associate_public_ip_address`",
			},
		},
		{
			name: "with savings",
			v: FinopsViolation{
				PolicyName:     "Right-size",
				PolicySlug:     "right-size",
				Address:        "aws_instance.web",
				Message:        "Oversized",
				MonthlySavings: rat.New(50),
				SavingsDetails: "Downgrade to t3.medium",
			},
			contains: []string{
				"**Potential Savings:** $50.00/mo",
				"Downgrade to t3.medium",
			},
		},
		{
			name: "with policy detail",
			v: FinopsViolation{
				PolicyName: "GP3 migration",
				PolicySlug: "gp3-migration",
				Address:    "aws_ebs_volume.data",
				Message:    "Migrate to gp3",
				PolicyDetail: &dashboard.PolicyDetail{
					ShortTitle:          "Migrate EBS to GP3",
					Risk:                "Low",
					RiskDescription:     "Minimal risk",
					Effort:              "Low",
					EffortDescription:   "Simple change",
					Downtime:            "None",
					DowntimeDescription: "No downtime required",
					AdditionalDetails:   "GP3 is cheaper and faster",
				},
			},
			contains: []string{
				"**Migrate EBS to GP3**",
				"**Risk** (Low): Minimal risk",
				"**Effort** (Low): Simple change",
				"**Downtime** (None): No downtime required",
				"GP3 is cheaper and faster",
			},
		},
		{
			name: "savings without details",
			v: FinopsViolation{
				PolicyName:     "Unused EIP",
				PolicySlug:     "unused-eip",
				Address:        "aws_eip.old",
				Message:        "Release unused EIP",
				MonthlySavings: rat.New(4),
			},
			contains: []string{
				"**Potential Savings:** $4.00/mo",
			},
			excludes: []string{"Downgrade"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildViolationMarkdown(tt.v)
			for _, s := range tt.contains {
				assert.Contains(t, got, s)
			}
			for _, s := range tt.excludes {
				assert.NotContains(t, got, s)
			}
		})
	}
}
