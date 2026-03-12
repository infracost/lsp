package lsp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPluralize(t *testing.T) {
	tests := []struct {
		name     string
		n        int
		singular string
		plural   string
		want     string
	}{
		{"zero uses plural", 0, "resource", "resources", "resources"},
		{"one uses singular", 1, "resource", "resources", "resource"},
		{"two uses plural", 2, "resource", "resources", "resources"},
		{"negative uses plural", -1, "resource", "resources", "resources"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pluralize(tt.n, tt.singular, tt.plural)
			assert.Equal(t, tt.want, got)
		})
	}
}
