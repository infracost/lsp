package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestScanTimeoutFromEnv(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"unset", "", 0},
		{"minutes", "15m", 15 * time.Minute},
		{"seconds", "90s", 90 * time.Second},
		{"unparseable falls back to the default", "banana", 0},
		{"missing unit falls back to the default", "600", 0},
		{"zero falls back to the default", "0s", 0},
		{"negative falls back to the default", "-5m", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("INFRACOST_LSP_SCAN_TIMEOUT", tt.env)
			assert.Equal(t, tt.want, scanTimeoutFromEnv())
		})
	}
}
