package scanner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	cliplugins "github.com/infracost/cli/pkg/plugins"
	repoconfig "github.com/infracost/config"
	pluginpb "github.com/infracost/proto/gen/go/infracost/plugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// deadlineSpyParser records the deadline the caller's context carried into Parse.
type deadlineSpyParser struct {
	pluginpb.ParserServiceClient

	called   bool
	hasDeadl bool
	budget   time.Duration
}

func (d *deadlineSpyParser) Parse(ctx context.Context, _ *pluginpb.ParseRequest, _ ...grpc.CallOption) (*pluginpb.ParseResponse, error) {
	d.called = true
	if deadline, ok := ctx.Deadline(); ok {
		d.hasDeadl = true
		d.budget = time.Until(deadline)
	}
	return &pluginpb.ParseResponse{}, nil
}

func scannerWithSpyParser(t *testing.T, spy *deadlineSpyParser) *Scanner {
	t.Helper()

	s := &Scanner{
		Plugins: &cliplugins.Config{
			LoadParserPluginForProject: func(context.Context, string) (*cliplugins.ParserPlugin, error) {
				return &cliplugins.ParserPlugin{ParserServiceClient: spy}, nil
			},
		},
	}
	s.Init()
	return s
}

func TestScanTimeoutOrDefault(t *testing.T) {
	tests := []struct {
		name    string
		scanner *Scanner
		want    time.Duration
	}{
		{"nil scanner", nil, DefaultScanTimeout},
		{"unset", &Scanner{}, DefaultScanTimeout},
		{"negative", &Scanner{ScanTimeout: -time.Second}, DefaultScanTimeout},
		{"custom", &Scanner{ScanTimeout: 90 * time.Second}, 90 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.scanner.ScanTimeoutOrDefault())
		})
	}
}

// The workspace setting wins over the baseline, and clearing it restores the
// baseline rather than dropping to the default. Clients send zero for a setting
// the user never set, so the clearing case is the common one.
func TestSetScanTimeoutPrecedence(t *testing.T) {
	s := &Scanner{ScanTimeout: 4 * time.Minute}

	assert.Equal(t, 4*time.Minute, s.ScanTimeoutOrDefault())

	s.SetScanTimeoutSeconds(int((12 * time.Minute).Seconds()))
	assert.Equal(t, 12*time.Minute, s.ScanTimeoutOrDefault())

	s.SetScanTimeoutSeconds(0)
	assert.Equal(t, 4*time.Minute, s.ScanTimeoutOrDefault())

	s.SetScanTimeoutSeconds(-60)
	assert.Equal(t, 4*time.Minute, s.ScanTimeoutOrDefault())

	bare := &Scanner{}
	bare.SetScanTimeoutSeconds(0)
	assert.Equal(t, DefaultScanTimeout, bare.ScanTimeoutOrDefault())
}

// initializationOptions is startup configuration, so it must set the baseline.
// If it set the override, the first didChangeConfiguration — which VS Code
// sends moments later, carrying nothing while the setting is undeclared — would
// wipe it before the cold first scan ran (FIX-619).
func TestBaselineScanTimeoutSurvivesClearedSetting(t *testing.T) {
	s := &Scanner{}
	s.Init()

	s.SetBaselineScanTimeoutSeconds(int((30 * time.Minute).Seconds()))
	assert.Equal(t, 30*time.Minute, s.ScanTimeoutOrDefault())

	s.SetScanTimeoutSeconds(0)
	assert.Equal(t, 30*time.Minute, s.ScanTimeoutOrDefault())

	// A non-positive baseline leaves the existing one alone.
	s.SetBaselineScanTimeoutSeconds(0)
	assert.Equal(t, 30*time.Minute, s.ScanTimeoutOrDefault())
}

// A budget under the parser's own per-download budget reintroduces the bug, and
// an unbounded one parks a project until the editor restarts.
func TestScanTimeoutIsClamped(t *testing.T) {
	tests := []struct {
		name string
		secs int
		want time.Duration
	}{
		{"below the floor is raised", 30, MinScanTimeout},
		{"above the ceiling is lowered", int((6 * time.Hour).Seconds()), MaxScanTimeout},
		{"absurd value cannot overflow to unset", 10_000_000_000, MaxScanTimeout},
		{"in range is kept", int((12 * time.Minute).Seconds()), 12 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Scanner{}
			s.Init()
			s.SetScanTimeoutSeconds(tt.secs)
			assert.Equal(t, tt.want, s.ScanTimeoutOrDefault())
		})
	}
}

func TestInitAppliesDefaultScanTimeout(t *testing.T) {
	s := &Scanner{}
	s.Init()
	assert.Equal(t, DefaultScanTimeout, s.ScanTimeout)

	// The default must clear the parser's own 3 minute per-download budget,
	// otherwise a cold module cache can never finish (FIX-619).
	assert.Greater(t, s.ScanTimeout, 3*time.Minute)

	// The floor applies to the environment baseline as well.
	tooSmall := &Scanner{ScanTimeout: time.Minute}
	tooSmall.Init()
	assert.Equal(t, MinScanTimeout, tooSmall.ScanTimeout)

	explicit := &Scanner{ScanTimeout: 20 * time.Minute}
	explicit.Init()
	assert.Equal(t, 20*time.Minute, explicit.ScanTimeout)
}

// parse must not impose a deadline of its own: the caller's per-project budget
// is the only one, so a slow cold-cache parse is not cut short at 60s.
func TestParseUsesCallerDeadline(t *testing.T) {
	project := &repoconfig.Project{Name: "proj", Path: "."}

	t.Run("caller deadline is passed through untouched", func(t *testing.T) {
		spy := &deadlineSpyParser{}
		s := scannerWithSpyParser(t, spy)

		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()

		_, err := s.parse(ctx, t.TempDir(), project, t.TempDir(), repoconfig.ProjectTypeTerraform, nil)
		require.NoError(t, err)
		require.True(t, spy.called)
		require.True(t, spy.hasDeadl)
		assert.Greater(t, spy.budget, 7*time.Minute)
	})

	t.Run("no caller deadline means no deadline", func(t *testing.T) {
		spy := &deadlineSpyParser{}
		s := scannerWithSpyParser(t, spy)

		_, err := s.parse(context.Background(), t.TempDir(), project, t.TempDir(), repoconfig.ProjectTypeTerraform, nil)
		require.NoError(t, err)
		require.True(t, spy.called)
		assert.False(t, spy.hasDeadl)
	})
}

func TestModuleCacheIsCold(t *testing.T) {
	assert.True(t, moduleCacheIsCold(filepath.Join(t.TempDir(), "does-not-exist")))

	empty := t.TempDir()
	assert.True(t, moduleCacheIsCold(empty))

	require.NoError(t, os.MkdirAll(filepath.Join(empty, "abc123"), 0o700))
	assert.False(t, moduleCacheIsCold(empty))
}
