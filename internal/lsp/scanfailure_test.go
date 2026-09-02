package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	cliplugins "github.com/infracost/cli/pkg/plugins"
	repoconfig "github.com/infracost/config"
	pluginpb "github.com/infracost/proto/gen/go/infracost/plugin"
	"google.golang.org/grpc"

	"github.com/owenrumney/go-lsp/lsp"
	"github.com/owenrumney/go-lsp/servertest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infracost/lsp/internal/api"
	"github.com/infracost/lsp/internal/scanner"
)

func TestIsDeadlineExceeded(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"context deadline", context.DeadlineExceeded, true},
		{"wrapped context deadline", fmt.Errorf("parsing: %w", context.DeadlineExceeded), true},
		{"grpc deadline", status.Error(codes.DeadlineExceeded, "context deadline exceeded"), true},
		{"wrapped grpc deadline", fmt.Errorf("parsing: %w", status.Error(codes.DeadlineExceeded, "boom")), true},
		{"cancelled", context.Canceled, false},
		{"other grpc error", status.Error(codes.Unavailable, "no plugin"), false},
		{"plain error", errors.New("bad hcl"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isDeadlineExceeded(tt.err))
		})
	}
}

func TestScanFailureMessage(t *testing.T) {
	timeout := scanFailureMessage("prod", fmt.Errorf("parsing: %w", status.Error(codes.DeadlineExceeded, "context deadline exceeded")))
	assert.Contains(t, timeout, "prod")
	assert.Contains(t, timeout, "timed out")
	assert.Contains(t, timeout, "module cache is warm")

	other := scanFailureMessage("prod", errors.New("bad hcl"))
	assert.Contains(t, other, "failed to scan prod")
	assert.Contains(t, other, "bad hcl")
	assert.NotContains(t, other, "timed out")
}

func TestScanFailureSummary(t *testing.T) {
	tests := []struct {
		name        string
		failed      []ProjectScanError
		total       int
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "single timeout",
			failed:      []ProjectScanError{{Project: "prod", TimedOut: true}},
			total:       4,
			wantContain: []string{"1 of 4", "prod", "timed out", "module cache is warm"},
		},
		{
			name:        "mixed causes still mentions the timeout advice",
			failed:      []ProjectScanError{{Project: "dev"}, {Project: "prod", TimedOut: true}},
			total:       2,
			wantContain: []string{"2 of 2", "dev, prod", "timed out"},
		},
		{
			name:        "no timeouts",
			failed:      []ProjectScanError{{Project: "dev"}},
			total:       3,
			wantContain: []string{"1 of 3", "dev", "See the Infracost output"},
			wantAbsent:  []string{"timed out"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scanFailureSummary(tt.failed, tt.total)
			for _, want := range tt.wantContain {
				assert.Contains(t, got, want)
			}
			for _, absent := range tt.wantAbsent {
				assert.NotContains(t, got, absent)
			}
		})
	}
}

// A failed scan has to be distinguishable from a project with nothing to cost,
// so the failure is carried in state rather than only logged (FIX-619).
func TestProjectScanErrorsReachStatus(t *testing.T) {
	srv := NewServer(nil, nil, api.NewTokenSource(nil))

	res, err := srv.HandleStatus(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 0, res.(StatusResult).FailedProjectCount)

	srv.setProjectError("prod", fmt.Errorf("parsing: %w", context.DeadlineExceeded))
	srv.setProjectError("dev", errors.New("bad hcl"))

	res, err = srv.HandleStatus(context.Background(), nil)
	require.NoError(t, err)
	st := res.(StatusResult)
	require.Equal(t, 2, st.FailedProjectCount)
	// Ordered by project name so the webview renders stably.
	assert.Equal(t, "dev", st.FailedProjects[0].Project)
	assert.False(t, st.FailedProjects[0].TimedOut)
	assert.Equal(t, "prod", st.FailedProjects[1].Project)
	assert.True(t, st.FailedProjects[1].TimedOut)

	srv.clearProjectError("prod")
	res, err = srv.HandleStatus(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 1, res.(StatusResult).FailedProjectCount)

	srv.clearResults()
	res, err = srv.HandleStatus(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 0, res.(StatusResult).FailedProjectCount)
}

// A project removed from the config must stop being reported as failing, or a
// stale failure outlives the project for the rest of the session (FIX-619).
func TestSetConfigPrunesErrorsForRemovedProjects(t *testing.T) {
	srv := NewServer(nil, nil, api.NewTokenSource(nil))
	srv.setProjectError("a", errors.New("boom"))
	srv.setProjectError("b", errors.New("boom"))

	srv.setConfig(&repoconfig.Config{Projects: []*repoconfig.Project{
		{Name: "a", Path: "."},
		{Name: "b", Path: "."},
	}})
	require.Len(t, srv.getProjectErrors(), 2)

	// "b" is deleted from infracost.yml, so its failure must go with it.
	srv.setConfig(&repoconfig.Config{Projects: []*repoconfig.Project{
		{Name: "a", Path: "."},
	}})
	errs := srv.getProjectErrors()
	require.Len(t, errs, 1)
	assert.Equal(t, "a", errs[0].Project)
}

// notifyContext must outlive the scan context, or the "scan failed" message and
// the terminal progress notification are dropped and the UI hangs (FIX-619).
func TestNotifyContextSurvivesDeadScanContext(t *testing.T) {
	scanCtx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, scanCtx.Err())

	nctx, ncancel := notifyContext(scanCtx)
	defer ncancel()

	assert.NoError(t, nctx.Err())
	_, ok := nctx.Deadline()
	assert.True(t, ok, "notification context should still be bounded")
}

// The workspace ceiling must bound the scan without ever being tighter than the
// budgets it hands out, or the later projects are starved (FIX-619).
func TestWorkspaceScanBudget(t *testing.T) {
	srv := NewServer(&scanner.Scanner{ScanTimeout: 10 * time.Minute}, nil, api.NewTokenSource(nil))

	tests := []struct {
		name     string
		projects int
		want     time.Duration
	}{
		{"zero projects is treated as one", 0, 10*time.Minute + workspaceScanSlack},
		{"one project", 1, 10*time.Minute + workspaceScanSlack},
		{"twenty projects", 20, 20 * (10*time.Minute + workspaceScanSlack)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := srv.workspaceScanBudget(tt.projects)
			assert.Equal(t, tt.want, got)

			// Never tighter than the sum of the per-project budgets it allows.
			projects := max(tt.projects, 1)
			assert.GreaterOrEqual(t, got, time.Duration(projects)*srv.scanner.ScanTimeoutOrDefault())
		})
	}
}

// A large configured budget must not overflow into a context that is already
// expired, which would abandon every project with only a log line.
func TestWorkspaceScanBudgetSaturates(t *testing.T) {
	srv := NewServer(&scanner.Scanner{ScanTimeout: scanner.MaxScanTimeout}, nil, api.NewTokenSource(nil))

	got := srv.workspaceScanBudget(1 << 40)
	assert.Equal(t, time.Duration(math.MaxInt64), got)
	assert.Positive(t, got)
}

// budgetSpyParser records the deadline each Parse call was given.
type budgetSpyParser struct {
	pluginpb.ParserServiceClient

	budgets []time.Duration
}

func (b *budgetSpyParser) Parse(ctx context.Context, _ *pluginpb.ParseRequest, _ ...grpc.CallOption) (*pluginpb.ParseResponse, error) {
	if deadline, ok := ctx.Deadline(); ok {
		b.budgets = append(b.budgets, time.Until(deadline))
	}
	return &pluginpb.ParseResponse{}, nil
}

// Every project must get its full budget under the workspace ceiling the scan
// loops actually pass, so an earlier project exhausting its own budget cannot
// truncate the projects behind it (FIX-619).
func TestScanProjectWithBudgetGivesEachProjectItsOwnDeadline(t *testing.T) {
	spy := &budgetSpyParser{}
	sc := &scanner.Scanner{
		ScanTimeout: 4 * time.Minute,
		TokenSource: api.NewTokenSource(nil),
		Plugins: &cliplugins.Config{
			LoadParserPluginForProject: func(context.Context, string) (*cliplugins.ParserPlugin, error) {
				return &cliplugins.ParserPlugin{ParserServiceClient: spy}, nil
			},
		},
	}
	sc.Init()

	srv := NewServer(sc, nil, api.NewTokenSource(nil))
	root := t.TempDir()
	cfg := &repoconfig.Config{Projects: []*repoconfig.Project{
		{Name: "a", Path: "."},
		{Name: "b", Path: "."},
		{Name: "c", Path: "."},
	}}

	// The workspace-budget context, exactly as the scan loops build it — a
	// WithTimeout parent, so a per-project deadline can be truncated by it if
	// the ceiling is sized wrong.
	parent, cancel := context.WithTimeout(context.Background(), srv.workspaceScanBudget(len(cfg.Projects)))
	defer cancel()

	for _, project := range cfg.Projects {
		// The scan fails after parse (no credentials); the budget is what matters.
		_, _ = srv.scanProjectWithBudget(parent, root, cfg, project)
		require.NoError(t, parent.Err(), "parent must survive a project's budget expiring")
	}

	require.Len(t, spy.budgets, len(cfg.Projects))
	for i, budget := range spy.budgets {
		assert.InDelta(t, (4 * time.Minute).Seconds(), budget.Seconds(), 5,
			"project %d should get the full budget", i)
	}
}

// Projects the loop never reached must be recorded as failures, or a workspace
// that ran out of budget renders them as "0 resources" (FIX-619).
func TestRecordAbandonedProjects(t *testing.T) {
	srv := NewServer(nil, nil, api.NewTokenSource(nil))

	abandoned := srv.recordAbandonedProjects([]*repoconfig.Project{
		{Name: "c", Path: "c"},
		{Name: "d", Path: "d"},
	})

	require.Len(t, abandoned, 2)
	for _, pe := range abandoned {
		assert.True(t, pe.TimedOut, "an abandoned project is a timeout")
		assert.Contains(t, pe.Message, "budget exhausted")
	}

	// And they reach the status the webview reads.
	res, err := srv.HandleStatus(context.Background(), nil)
	require.NoError(t, err)
	st := res.(StatusResult)
	assert.Equal(t, 2, st.FailedProjectCount)

	// The summary names them, so the user is told rather than shown zeroes.
	summary := scanFailureSummary(abandoned, 4)
	assert.Contains(t, summary, "2 of 4")
	assert.Contains(t, summary, "c, d")
	assert.Contains(t, summary, "timed out")
}

// The cold-cache hint has to be sampled once per workspace scan: project one
// populates the shared cache directory, so a per-project check would flag only
// project one while the rest are still downloading (FIX-619).
func TestScanTitle(t *testing.T) {
	assert.Equal(t, "Scanning prod (warming module cache)...", scanTitle("prod", true))
	assert.Equal(t, "Scanning prod...", scanTitle("prod", false))
}

// gateParser blocks each parse until its context is cancelled, so a test can
// stop a workspace scan at a known point in the loop.
type gateParser struct {
	pluginpb.ParserServiceClient

	started chan string
}

func (g *gateParser) Parse(ctx context.Context, req *pluginpb.ParseRequest, _ ...grpc.CallOption) (*pluginpb.ParseResponse, error) {
	g.started <- req.GetPath()
	<-ctx.Done()
	return nil, status.FromContextError(ctx.Err()).Err()
}

// A cancelled workspace scan must not claim the projects it never reached timed
// out. Only an exhausted budget means that; a cancellation means a replacement
// scan is running and reports for itself (FIX-619).
func TestCancelledWorkspaceScanReportsNothing(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, name), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(root, name, "main.tf"), []byte("# empty\n"), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "infracost.yml"), []byte(
		"version: 0.1\nprojects:\n  - path: a\n    name: a\n  - path: b\n    name: b\n  - path: c\n    name: c\n",
	), 0o600))

	gate := &gateParser{started: make(chan string, 4)}
	sc := &scanner.Scanner{
		TokenSource: api.NewTokenSource(nil),
		Plugins: &cliplugins.Config{
			LoadParserPluginForProject: func(context.Context, string) (*cliplugins.ParserPlugin, error) {
				return &cliplugins.ParserPlugin{ParserServiceClient: gate}, nil
			},
		},
	}
	sc.Init()

	srv := NewServer(sc, nil, api.NewTokenSource(nil))
	h := servertest.New(t, srv, servertest.WithInitializeParams(&lsp.InitializeParams{
		InitializationOptions: json.RawMessage(`{"checkForUpdates":false}`),
	}))
	// After initialize, which resolves the workspace root itself.
	srv.workspaceRoot = root

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.analyzeFullScan(pathToURI(filepath.Join(root, "a", "main.tf")))
	}()

	// Cancel once the loop is inside the first project, so it reaches the
	// budget check for the second with a cancelled context.
	select {
	case <-gate.started:
	case <-time.After(30 * time.Second):
		t.Fatal("first project never started scanning")
	}
	srv.cancelWorkspaceScan()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("analyzeFullScan did not return after cancellation")
	}

	// No user-facing warning: nothing failed on its own merits.
	assert.Empty(t, h.Messages(), "a cancelled scan must not report failures")

	// And the projects it never reached are not recorded as timed out.
	for _, pe := range srv.getProjectErrors() {
		assert.NotEqual(t, "b", pe.Project, "project b was never scanned")
		assert.NotEqual(t, "c", pe.Project, "project c was never scanned")
		assert.False(t, pe.TimedOut, "cancellation is not a timeout")
	}
}

// The fallback loads config without the org's config template, so it must stand
// aside rather than cancel a real workspace scan (FIX-619).
func TestFallbackScanDoesNotSupersedeWorkspaceScan(t *testing.T) {
	srv := NewServer(nil, nil, api.NewTokenSource(nil))

	ctx, cancel, version, ok := srv.tryBeginWorkspaceScan()
	require.True(t, ok)
	defer srv.endWorkspaceScan(version, cancel)

	_, _, _, ok = srv.tryBeginWorkspaceScan()
	assert.False(t, ok, "a second fallback must stand aside")
	require.NoError(t, ctx.Err(), "the running scan must not be cancelled")

	// Once it finishes, the next one may start.
	srv.endWorkspaceScan(version, cancel)
	_, cancel2, version2, ok := srv.tryBeginWorkspaceScan()
	assert.True(t, ok)
	srv.endWorkspaceScan(version2, cancel2)
}

// beginWorkspaceScan has to bump the version as well as cancel, or the
// superseded scan passes every isCurrentScanVersion check and reports over the
// top of its replacement (FIX-619).
func TestBeginWorkspaceScanSupersedesPredecessor(t *testing.T) {
	srv := NewServer(nil, nil, api.NewTokenSource(nil))

	firstCtx, firstCancel, firstVersion := srv.beginWorkspaceScan()
	defer srv.endWorkspaceScan(firstVersion, firstCancel)
	require.True(t, srv.isCurrentScanVersion(firstVersion))
	require.False(t, srv.scanSuperseded(firstCtx, firstVersion))

	secondCtx, secondCancel, secondVersion := srv.beginWorkspaceScan()
	defer srv.endWorkspaceScan(secondVersion, secondCancel)

	assert.Error(t, firstCtx.Err(), "the predecessor must be cancelled")
	assert.True(t, srv.scanSuperseded(firstCtx, firstVersion))
	assert.False(t, srv.scanSuperseded(secondCtx, secondVersion))
}
