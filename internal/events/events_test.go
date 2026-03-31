package events

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPush(t *testing.T) {
	tests := []struct {
		name      string
		extra     []interface{}
		assertReq func(t *testing.T, r *http.Request, env map[string]interface{})
	}{
		{
			name: "sends correct payload with metadata",
			assertReq: func(t *testing.T, r *http.Request, env map[string]interface{}) {
				assert.Equal(t, "/event", r.URL.Path)
				assert.Equal(t, "infracost-ls", env["caller"])
				assert.NotEmpty(t, env["os"])
				assert.NotEmpty(t, env["arch"])
				assert.NotEmpty(t, env["installId"])
				assert.Equal(t, env["id"], env["installId"])
			},
		},
		{
			name:  "merges extra fields",
			extra: []interface{}{"runSeconds", 1.5, "totalResources", 3},
			assertReq: func(t *testing.T, _ *http.Request, env map[string]interface{}) {
				assert.Equal(t, 1.5, env["runSeconds"])
				assert.Equal(t, float64(3), env["totalResources"])
				assert.NotEmpty(t, env["installId"])
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var captured struct {
				req *http.Request
				env map[string]interface{}
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured.req = r
				body, _ := io.ReadAll(r.Body)
				var payload map[string]interface{}
				require.NoError(t, json.Unmarshal(body, &payload))
				assert.Equal(t, "test-event", payload["event"])
				captured.env, _ = payload["env"].(map[string]interface{})
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			c := NewClient(http.DefaultClient, srv.URL)
			c.Push(context.Background(), "test-event", tt.extra...)

			require.NotNil(t, captured.req)
			tt.assertReq(t, captured.req, captured.env)
		})
	}
}

func TestGetMetadata(t *testing.T) {
	RegisterMetadata("testKey", "string-value")
	defer func() {
		metadataMu.Lock()
		delete(metadata, "testKey")
		metadataMu.Unlock()
	}()

	tests := []struct {
		name   string
		key    string
		wantOK bool
		want   string
	}{
		{name: "existing key", key: "testKey", wantOK: true, want: "string-value"},
		{name: "missing key", key: "nonexistent", wantOK: false},
		{name: "override via RegisterMetadata", key: "caller", wantOK: true, want: "infracost-ls"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, ok := GetMetadata[string](tt.key)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.want, val)
			}
		})
	}
}

func TestGetMetadata_TypeMismatch(t *testing.T) {
	RegisterMetadata("typedKey", "string-value")
	defer func() {
		metadataMu.Lock()
		delete(metadata, "typedKey")
		metadataMu.Unlock()
	}()

	_, ok := GetMetadata[int]("typedKey")
	assert.False(t, ok)
}
