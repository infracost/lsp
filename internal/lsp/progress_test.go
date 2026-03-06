package lsp

import (
	"context"
	"testing"
)

func TestProgressReporterNilClient(_ *testing.T) {
	p := newProgressReporter(nil)

	ctx := context.Background()
	// Should not panic with nil client.
	p.Begin(ctx, "test")
	p.Report(ctx, "working...", 50)
	p.End(ctx, "done")
}

func TestProgressReporterEndWithoutBegin(_ *testing.T) {
	p := newProgressReporter(nil)

	ctx := context.Background()
	// End without Begin should be a no-op (active=false).
	p.End(ctx, "done")
	p.Report(ctx, "working...", 50)
}
