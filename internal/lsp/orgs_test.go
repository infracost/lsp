package lsp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/infracost/cli/pkg/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/infracost/lsp/internal/api"
	"github.com/infracost/lsp/internal/scanner"
)

// writeUserCache saves uc to a temp file and sets the env var so newAuthConfig
// picks it up instead of the real user cache.
func writeUserCache(t *testing.T, uc *auth.UserCache) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "user.json")
	t.Setenv("INFRACOST_CLI_USER_CACHE_PATH", path)
	cfg := &auth.Config{}
	cfg.UserCachePath = path
	require.NoError(t, cfg.SaveUserCache(uc))
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return NewServer(&scanner.Scanner{}, nil, api.NewTokenSource(nil))
}

func TestHandleOrgs_NoCache(t *testing.T) {
	t.Setenv("INFRACOST_CLI_USER_CACHE_PATH", filepath.Join(t.TempDir(), "user.json"))
	s := newTestServer(t)

	got, err := s.HandleOrgs(context.Background(), nil)
	require.NoError(t, err)

	info, ok := got.(*OrgInfo)
	require.True(t, ok)
	assert.Empty(t, info.Organizations)
	assert.Empty(t, info.SelectedOrgID)
}

func TestHandleOrgs_SingleOrg(t *testing.T) {
	writeUserCache(t, &auth.UserCache{
		Organizations: []auth.CachedOrganization{
			{ID: "org-1", Name: "Acme", Slug: "acme"},
		},
	})
	s := newTestServer(t)

	got, err := s.HandleOrgs(context.Background(), nil)
	require.NoError(t, err)

	info, ok := got.(*OrgInfo)
	require.True(t, ok)
	require.Len(t, info.Organizations, 1)
	assert.Equal(t, "org-1", info.Organizations[0].ID)
	assert.Equal(t, "Acme", info.Organizations[0].Name)
	assert.Equal(t, "acme", info.Organizations[0].Slug)
	// Falls back to first org when nothing is selected.
	assert.Equal(t, "org-1", info.SelectedOrgID)
}

func TestHandleOrgs_SelectedOrgFromCache(t *testing.T) {
	writeUserCache(t, &auth.UserCache{
		Organizations: []auth.CachedOrganization{
			{ID: "org-1", Name: "Acme", Slug: "acme"},
			{ID: "org-2", Name: "Globex", Slug: "globex"},
		},
		SelectedOrgID: "org-2",
	})
	s := newTestServer(t)

	got, err := s.HandleOrgs(context.Background(), nil)
	require.NoError(t, err)

	info, ok := got.(*OrgInfo)
	require.True(t, ok)
	assert.Equal(t, "org-2", info.SelectedOrgID)
}

func TestHandleOrgs_LocalOrgOverridesCache(t *testing.T) {
	root := t.TempDir()
	writeUserCache(t, &auth.UserCache{
		Organizations: []auth.CachedOrganization{
			{ID: "org-1", Name: "Acme", Slug: "acme"},
			{ID: "org-2", Name: "Globex", Slug: "globex"},
		},
		SelectedOrgID: "org-2",
	})

	// Write a local org file pointing to acme.
	require.NoError(t, auth.WriteLocalOrg(root, "acme"))

	s := newTestServer(t)
	s.workspaceRoot = root

	got, err := s.HandleOrgs(context.Background(), nil)
	require.NoError(t, err)

	info, ok := got.(*OrgInfo)
	require.True(t, ok)
	// Local file takes priority over selectedOrgId in user cache.
	assert.Equal(t, "org-1", info.SelectedOrgID)
}

func TestHandleSelectOrg(t *testing.T) {
	root := t.TempDir()
	writeUserCache(t, &auth.UserCache{
		Organizations: []auth.CachedOrganization{
			{ID: "org-1", Name: "Acme", Slug: "acme"},
			{ID: "org-2", Name: "Globex", Slug: "globex"},
		},
	})

	s := newTestServer(t)
	s.workspaceRoot = root

	params, err := json.Marshal(map[string]string{"orgId": "org-2"})
	require.NoError(t, err)

	got, err := s.HandleSelectOrg(context.Background(), params)
	require.NoError(t, err)

	info, ok := got.(*OrgInfo)
	require.True(t, ok)
	assert.Equal(t, "org-2", info.SelectedOrgID)

	// Verify the slug was written to the local org file.
	slug, err := auth.ReadLocalOrg(root)
	require.NoError(t, err)
	assert.Equal(t, "globex", slug)
}

func TestHandleSelectOrg_NotLoggedIn(t *testing.T) {
	t.Setenv("INFRACOST_CLI_USER_CACHE_PATH", filepath.Join(t.TempDir(), "user.json"))
	s := newTestServer(t)
	s.workspaceRoot = t.TempDir()

	params, err := json.Marshal(map[string]string{"orgId": "org-1"})
	require.NoError(t, err)

	_, err = s.HandleSelectOrg(context.Background(), params)
	assert.ErrorContains(t, err, "not logged in")
}

func TestHandleSelectOrg_OrgNotFound(t *testing.T) {
	writeUserCache(t, &auth.UserCache{
		Organizations: []auth.CachedOrganization{
			{ID: "org-1", Name: "Acme", Slug: "acme"},
		},
	})
	s := newTestServer(t)
	s.workspaceRoot = t.TempDir()

	params, err := json.Marshal(map[string]string{"orgId": "org-999"})
	require.NoError(t, err)

	_, err = s.HandleSelectOrg(context.Background(), params)
	assert.ErrorContains(t, err, `org "org-999" not found`)
}

func TestHandleSelectOrg_NoWorkspaceRoot(t *testing.T) {
	writeUserCache(t, &auth.UserCache{
		Organizations: []auth.CachedOrganization{
			{ID: "org-1", Name: "Acme", Slug: "acme"},
		},
	})
	s := newTestServer(t)
	// workspaceRoot deliberately left empty.

	params, err := json.Marshal(map[string]string{"orgId": "org-1"})
	require.NoError(t, err)

	_, err = s.HandleSelectOrg(context.Background(), params)
	assert.ErrorContains(t, err, "no workspace root")
}

func TestHandleSelectOrg_InvalidParams(t *testing.T) {
	s := newTestServer(t)

	_, err := s.HandleSelectOrg(context.Background(), json.RawMessage(`not json`))
	assert.Error(t, err)
}
