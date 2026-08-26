package lsp

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyBicepSetting(t *testing.T) {
	enabled := true
	disabled := false

	tests := []struct {
		name string
		// ambient is what the user exported before the editor started. The
		// empty string means the variable is unset.
		ambient string
		setting *bool
		want    string
	}{
		{
			name:    "nil leaves an exported value alone",
			ambient: "true",
			setting: nil,
			want:    "true",
		},
		{
			name:    "nil leaves an unset variable unset",
			ambient: "",
			setting: nil,
			want:    "",
		},
		{
			name:    "true sets the variable",
			ambient: "",
			setting: &enabled,
			want:    "true",
		},
		// The two cases below are why the setting wins over the environment
		// rather than merely filling a gap in it: in an editor the setting is
		// the user's machine-scoped decision, and a client switched off should
		// not keep pricing Bicep because something upstream of the editor
		// exported the variable.
		{
			name:    "false unsets an ambient value",
			ambient: "true",
			setting: &disabled,
			want:    "",
		},
		{
			name:    "false on an already-unset variable is a no-op",
			ambient: "",
			setting: &disabled,
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Setenv restores the prior value — including "unset" — when the
			// subtest ends, so cases can't leak into each other.
			t.Setenv(bicepEnabledEnv, tt.ambient)
			if tt.ambient == "" {
				// t.Setenv can only set, so an "unset" ambient has to be
				// established afterwards; the cleanup it registered still runs.
				assert.NoError(t, os.Unsetenv(bicepEnabledEnv))
			}

			applyBicepSetting(tt.setting)

			got, ok := os.LookupEnv(bicepEnabledEnv)
			if tt.want == "" {
				assert.False(t, ok, "expected %s to be unset, got %q", bicepEnabledEnv, got)
				return
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBicepEnabled(t *testing.T) {
	// The accepted spellings must stay in step with the ARM plugin's own
	// bicepEnabled: the plugin inherits this process's environment, so a value
	// one side reads as "on" and the other as "off" means the server analyzes
	// files the plugin ignores, or the reverse.
	// The leading space in " true" is the point — the value is trimmed before
	// it is compared — so gocritic's suspicious-whitespace check is muted here.
	tests := map[string]bool{ //nolint:gocritic
		"true":  true,
		"TRUE":  true,
		" true": true,
		"1":     true,
		"yes":   true,
		"on":    true,
		"false": false,
		"0":     false,
		"":      false,
		"maybe": false,
	}

	for value, want := range tests {
		t.Run("value="+value, func(t *testing.T) {
			t.Setenv(bicepEnabledEnv, value)
			assert.Equal(t, want, bicepEnabled())
		})
	}
}

func TestIsSupportedFile_Bicep(t *testing.T) {
	files := []string{"file:///repo/main.bicep", "file:///repo/prod.bicepparam"}

	t.Run("gate off", func(t *testing.T) {
		t.Setenv(bicepEnabledEnv, "")
		require.NoError(t, os.Unsetenv(bicepEnabledEnv))

		for _, uri := range files {
			assert.False(t, isSupportedFile(uri), uri)
		}
		// Generated ARM JSON is priced with the gate off, so it must stay
		// supported either way — the gate is about compiling Bicep, not about
		// ARM.
		assert.True(t, isSupportedFile("file:///repo/azuredeploy.json"))
	})

	t.Run("gate on", func(t *testing.T) {
		t.Setenv(bicepEnabledEnv, "true")

		for _, uri := range files {
			assert.True(t, isSupportedFile(uri), uri)
		}
	})
}
