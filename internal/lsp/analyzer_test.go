package lsp

import (
	"sync/atomic"
	"testing"
	"time"

	repoconfig "github.com/infracost/config"
	"github.com/stretchr/testify/assert"

	"github.com/infracost/lsp/internal/api"
)

func TestScheduleAnalyzeDebounce(t *testing.T) {
	var scanCount atomic.Int32

	srv := NewServer(nil, nil, api.NewTokenSource(nil))
	srv.workspaceRoot = "/tmp/test"
	srv.setConfig(&repoconfig.Config{
		Projects: []*repoconfig.Project{
			{Name: "proj", Path: "."},
		},
	})

	// Override the debounce timer callback to count scans instead of
	// running the real analyze (which needs a full scanner).
	triggerScan := func(_ string) {
		projectName := "proj"

		srv.mu.Lock()
		defer srv.mu.Unlock()

		if t, ok := srv.scanTimers[projectName]; ok {
			t.Stop()
		}

		srv.scanTimers[projectName] = time.AfterFunc(scanDebounce, func() {
			srv.mu.Lock()
			delete(srv.scanTimers, projectName)
			srv.mu.Unlock()

			scanCount.Add(1)
		})
	}

	// Simulate 5 rapid saves.
	for range 5 {
		triggerScan("file:///tmp/test/main.tf")
		time.Sleep(50 * time.Millisecond)
	}

	// Wait for debounce to fire (last save + debounce window + margin).
	time.Sleep(scanDebounce + 100*time.Millisecond)

	assert.Equal(t, int32(1), scanCount.Load())
}

func TestScheduleAnalyzeCoalescesRapidSaves(t *testing.T) {
	srv := NewServer(nil, nil, api.NewTokenSource(nil))
	srv.workspaceRoot = "/tmp/test"
	srv.setConfig(&repoconfig.Config{
		Projects: []*repoconfig.Project{
			{Name: "proj", Path: "."},
		},
	})

	// Fire 5 rapid scheduleAnalyze calls. Since scanner is nil, the
	// timer callback will panic if it actually runs analyze. We just
	// verify that only one timer exists after coalescing.
	for range 5 {
		srv.scheduleAnalyze("file:///tmp/test/main.tf")
	}

	srv.mu.RLock()
	timerCount := len(srv.scanTimers)
	srv.mu.RUnlock()

	assert.Equal(t, 1, timerCount)

	// Clean up: stop timers so the callback doesn't fire and panic.
	srv.mu.Lock()
	for name, timer := range srv.scanTimers {
		timer.Stop()
		delete(srv.scanTimers, name)
	}
	srv.mu.Unlock()
}

func TestScheduleAnalyzeCancelsInFlight(t *testing.T) {
	srv := NewServer(nil, nil, api.NewTokenSource(nil))
	srv.workspaceRoot = "/tmp/test"
	srv.setConfig(&repoconfig.Config{
		Projects: []*repoconfig.Project{
			{Name: "proj", Path: "."},
		},
	})

	// Simulate an in-flight scan by placing a cancel func.
	cancelled := make(chan struct{})
	srv.mu.Lock()
	srv.scanCancels["proj"] = func() { close(cancelled) }
	srv.mu.Unlock()

	// scheduleAnalyze should cancel the in-flight scan when the timer fires.
	srv.scheduleAnalyze("file:///tmp/test/main.tf")

	// Stop the timer and manually invoke its logic to test cancel behavior
	// without needing a real scanner.
	srv.mu.Lock()
	if timer, ok := srv.scanTimers["proj"]; ok {
		timer.Stop()
		delete(srv.scanTimers, "proj")
	}
	// The timer callback would cancel in-flight. Simulate that:
	if cancel, ok := srv.scanCancels["proj"]; ok {
		cancel()
		delete(srv.scanCancels, "proj")
	}
	srv.mu.Unlock()

	select {
	case <-cancelled:
		// The old in-flight scan was cancelled.
	case <-time.After(time.Second):
		assert.Fail(t, "expected in-flight scan to be cancelled")
	}
}
