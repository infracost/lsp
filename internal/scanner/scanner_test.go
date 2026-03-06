package scanner

import (
	"testing"
)

func TestTopLevelModulePrefix(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"aws_instance.foo", ""},
		{"module.dashboard.aws_rds_cluster_instance.foo", "module.dashboard"},
		{"module.base.module.eks.aws_eks_cluster.this", "module.base"},
		{"module.dashboard", ""},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := topLevelModulePrefix(tt.name)
			if got != tt.want {
				t.Errorf("topLevelModulePrefix(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}
