package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/owenrumney/go-lsp/server"
)

// ExportTraceParams are optional knobs supplied by the client. Pointers let the
// client distinguish "unset" from explicit false; defaults are applied when nil.
type ExportTraceParams struct {
	RedactDocumentText *bool `json:"redactDocumentText,omitempty"`
	RedactFilePaths    *bool `json:"redactFilePaths,omitempty"`
	RedactLogs         *bool `json:"redactLogs,omitempty"`
	Pretty             *bool `json:"pretty,omitempty"`
}

// ExportTraceResult is the response payload for infracost/exportTrace.
type ExportTraceResult struct {
	Available bool            `json:"available"`
	Trace     json.RawMessage `json:"trace,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// HandleExportTrace returns the in-memory go-lsp debug trace as embedded JSON.
// Defaults redact document text and file paths so the result is safe to attach
// to a support bundle without leaking source or local filesystem layout.
func (s *Server) HandleExportTrace(_ context.Context, params json.RawMessage) (any, error) {
	if s.srv == nil {
		return ExportTraceResult{Available: false, Error: "server not initialized"}, nil
	}

	opts := server.TraceExportOptions{
		RedactDocumentText: true,
		RedactFilePaths:    true,
		RedactLogs:         false,
		Pretty:             false,
	}

	if len(params) > 0 && string(params) != "null" {
		var p ExportTraceParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("parsing exportTrace params: %w", err)
		}
		if p.RedactDocumentText != nil {
			opts.RedactDocumentText = *p.RedactDocumentText
		}
		if p.RedactFilePaths != nil {
			opts.RedactFilePaths = *p.RedactFilePaths
		}
		if p.RedactLogs != nil {
			opts.RedactLogs = *p.RedactLogs
		}
		if p.Pretty != nil {
			opts.Pretty = *p.Pretty
		}
	}

	data, err := s.srv.ExportDebugTrace(opts)
	if err != nil {
		if errors.Is(err, server.ErrDebugTraceUnavailable) {
			return ExportTraceResult{Available: false, Error: "debug capture not enabled"}, nil
		}
		return nil, fmt.Errorf("exporting debug trace: %w", err)
	}

	return ExportTraceResult{Available: true, Trace: data}, nil
}
