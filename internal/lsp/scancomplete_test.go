package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	cliplugins "github.com/infracost/cli/pkg/plugins"
	repoconfig "github.com/infracost/config"
	pluginpb "github.com/infracost/proto/gen/go/infracost/plugin"
	"github.com/owenrumney/go-lsp/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/infracost/lsp/internal/api"
	"github.com/infracost/lsp/internal/scanner"
)

// notifRecorder records the notification methods a Server sends on the wire.
// servertest drops notifications it does not recognise, so scanComplete — the
// one thing that clears the webview's scanning state — must be read directly.
type notifRecorder struct {
	mu      sync.Mutex
	cond    *sync.Cond
	methods []string
	// ready closes once the server answers a request, proving it has injected
	// the client into the handler.
	ready     chan struct{}
	readyOnce sync.Once
}

func (r *notifRecorder) add(method string) {
	r.mu.Lock()
	r.methods = append(r.methods, method)
	r.cond.Broadcast()
	r.mu.Unlock()
}

// wait blocks until method has been seen, or the context is done.
func (r *notifRecorder) wait(ctx context.Context, method string) error {
	// Wake the waiter when the context is done; sync.Cond has no deadline.
	stop := context.AfterFunc(ctx, func() {
		r.mu.Lock()
		r.cond.Broadcast()
		r.mu.Unlock()
	})
	defer stop()

	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		for _, m := range r.methods {
			if m == method {
				return nil
			}
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for %s, saw %v: %w", method, r.methods, err)
		}
		r.cond.Wait()
	}
}

// recordNotifications runs srv as a real LSP server over an in-memory pipe,
// answering every request with null so progress tokens work without an editor.
func recordNotifications(t *testing.T, srv *Server) *notifRecorder {
	t.Helper()

	clientConn, serverConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())

	go func() { _ = server.NewServer(srv).Run(ctx, serverConn) }()

	r := &notifRecorder{ready: make(chan struct{})}
	r.cond = sync.NewCond(&r.mu)

	go func() {
		reader := bufio.NewReader(clientConn)
		for {
			body, err := readFrame(reader)
			if err != nil {
				return
			}

			var msg struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(body, &msg); err != nil {
				continue
			}
			if msg.Method == "" {
				// A response to our handshake.
				r.readyOnce.Do(func() { close(r.ready) })
				continue
			}
			if msg.ID == nil {
				r.add(msg.Method)
				continue
			}
			// A request: reply, or the server blocks waiting on us.
			reply := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":null}`, msg.ID)
			if _, err := fmt.Fprintf(clientConn, "Content-Length: %d\r\n\r\n%s", len(reply), reply); err != nil {
				return
			}
		}
	}()

	t.Cleanup(func() {
		cancel()
		_ = clientConn.Close()
	})

	// Handshake: sendScanComplete is a no-op until the client is injected. No
	// workspace root and no update check, so initialize starts nothing.
	init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":` +
		`{"processId":1,"capabilities":{},"initializationOptions":{"checkForUpdates":false}}}`
	_, err := fmt.Fprintf(clientConn, "Content-Length: %d\r\n\r\n%s", len(init), init)
	require.NoError(t, err)

	select {
	case <-r.ready:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not answer initialize")
	}
	return r
}

func readFrame(reader *bufio.Reader) ([]byte, error) {
	length := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			if length, err = strconv.Atoi(strings.TrimSpace(v)); err != nil {
				return nil, err
			}
		}
	}
	if length == 0 {
		return nil, fmt.Errorf("frame with no content length")
	}

	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, err
	}
	return body, nil
}

// failingParser makes every parse fail the way the reported bug did.
type failingParser struct {
	pluginpb.ParserServiceClient
}

func (failingParser) Parse(context.Context, *pluginpb.ParseRequest, ...grpc.CallOption) (*pluginpb.ParseResponse, error) {
	return nil, status.Error(codes.DeadlineExceeded, "context deadline exceeded")
}

// A failed scan must still send infracost/scanComplete: it is the only thing
// that clears hasCompletedScan, so without it the webview shows "scanning the
// workspace" until restart — the headline symptom of FIX-619.
func TestFailedScanStillSendsScanComplete(t *testing.T) {
	sc := &scanner.Scanner{
		TokenSource: api.NewTokenSource(nil),
		Plugins: &cliplugins.Config{
			LoadParserPluginForProject: func(context.Context, string) (*cliplugins.ParserPlugin, error) {
				return &cliplugins.ParserPlugin{ParserServiceClient: failingParser{}}, nil
			},
		},
	}
	sc.Init()

	srv := NewServer(sc, nil, api.NewTokenSource(nil))
	recorder := recordNotifications(t, srv)

	// After the handshake: initialize resolves the workspace root itself.
	root := t.TempDir()
	srv.workspaceRoot = root
	srv.setConfig(&repoconfig.Config{Projects: []*repoconfig.Project{{Name: "prod", Path: "."}}})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	srv.analyze(ctx, pathToURI(filepath.Join(root, "main.tf")))

	require.NoError(t, recorder.wait(ctx, "infracost/scanComplete"))

	// And the failure is recorded, not merely logged.
	errs := srv.getProjectErrors()
	require.Len(t, errs, 1)
	assert.Equal(t, "prod", errs[0].Project)
	assert.True(t, errs[0].TimedOut)
}
