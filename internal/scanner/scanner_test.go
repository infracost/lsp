package scanner

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTopLevelModulePrefix(t *testing.T) {
	tests := []struct {
		name            string
		resourceAddress string
		want            string
	}{
		{
			"aws_instance.foo",
			"aws_instance.foo",
			"",
		},
		{
			"module.dashboard.aws_rds_cluster_instance.foo",
			"module.dashboard.aws_rds_cluster_instance.foo",
			"module.dashboard",
		},
		{
			"module.base.module.eks.aws_eks_cluster.this",
			"module.base.module.eks.aws_eks_cluster.this",
			"module.base",
		},
		{
			"module.dashboard",
			"module.dashboard",
			"",
		},
		{"no input", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := topLevelModulePrefix(tt.name)
			assert.Equal(t, tt.want, got)
		})
	}
}
