package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/infracost/cli/pkg/auth"
	"github.com/infracost/cli/pkg/environment"
	"github.com/infracost/lsp/internal/api"
	"github.com/infracost/lsp/internal/dashboard"
)

// OrgEntry represents a single organization.
type OrgEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// OrgInfo is the response type for infracost/orgs and infracost/selectOrg.
type OrgInfo struct {
	Organizations        []OrgEntry `json:"organizations"`
	SelectedOrgID        string     `json:"selectedOrgId"`
	HasExplicitSelection bool       `json:"hasExplicitSelection"`
}

type orgsParams struct {
	Refresh bool `json:"refresh"`
}

// HandleOrgs returns the list of organizations the user belongs to and the
// currently active org, resolved in priority order:
//  1. .infracost/org in the workspace root (repo-scoped)
//  2. selectedOrgId in user.json (global)
//  3. first org in the list
func (s *Server) HandleOrgs(ctx context.Context, params json.RawMessage) (any, error) {
	var p orgsParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("orgs: invalid params: %w", err)
		}
	}
	if p.Refresh {
		if err := s.refreshUserCache(ctx); err != nil {
			return nil, fmt.Errorf("orgs: refreshing user cache: %w", err)
		}
	}
	return s.loadOrgInfo()
}

// HandleSelectOrg writes the selected org slug to .infracost/org in the
// workspace root and returns the updated OrgInfo.
func (s *Server) HandleSelectOrg(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		OrgID string `json:"orgId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("selectOrg: invalid params: %w", err)
	}

	uc, err := newAuthConfig().LoadUserCache()
	if err != nil {
		return nil, fmt.Errorf("selectOrg: loading user cache: %w", err)
	}
	if uc == nil {
		return nil, fmt.Errorf("selectOrg: not logged in")
	}

	var slug string
	for _, o := range uc.Organizations {
		if o.ID == p.OrgID {
			slug = o.Slug
			break
		}
	}
	if slug == "" {
		return nil, fmt.Errorf("selectOrg: org %q not found", p.OrgID)
	}

	s.mu.RLock()
	root := s.workspaceRoot
	s.mu.RUnlock()

	if root == "" {
		return nil, fmt.Errorf("selectOrg: no workspace root")
	}

	if err := auth.WriteLocalOrg(root, slug); err != nil {
		return nil, fmt.Errorf("selectOrg: %w", err)
	}

	slog.Info("selectOrg: wrote local org", "slug", slug, "root", root)

	return s.loadOrgInfo()
}

// loadOrgInfo reads the user cache and resolves the active org.
func (s *Server) loadOrgInfo() (*OrgInfo, error) {
	uc, err := newAuthConfig().LoadUserCache()
	if err != nil {
		return nil, fmt.Errorf("orgs: loading user cache: %w", err)
	}
	if uc == nil || len(uc.Organizations) == 0 {
		return &OrgInfo{Organizations: []OrgEntry{}}, nil
	}

	orgs := make([]OrgEntry, len(uc.Organizations))
	for i, o := range uc.Organizations {
		orgs[i] = OrgEntry{ID: o.ID, Name: o.Name, Slug: o.Slug}
	}

	selectedID := uc.SelectedOrgID
	if selectedID != "" && !cachedOrgIDExists(uc.Organizations, selectedID) {
		selectedID = ""
	}
	hasExplicit := selectedID != ""

	s.mu.RLock()
	root := s.workspaceRoot
	s.mu.RUnlock()

	if root != "" {
		localSlug, err := auth.ReadLocalOrg(root)
		if err != nil {
			slog.Warn("orgs: failed to read local org file", "error", err)
		} else if localSlug != "" {
			for _, o := range uc.Organizations {
				if o.Slug == localSlug {
					selectedID = o.ID
					hasExplicit = true
					break
				}
			}
		}
	}

	if selectedID == "" && len(orgs) > 0 {
		selectedID = orgs[0].ID
	}

	return &OrgInfo{
		Organizations:        orgs,
		SelectedOrgID:        selectedID,
		HasExplicitSelection: hasExplicit,
	}, nil
}

// refreshUserCache fetches the current user's profile from the API and writes
// it to user.json, called after a successful device auth flow.
func (s *Server) refreshUserCache(ctx context.Context) error {
	if s.scanner == nil {
		return fmt.Errorf("scanner not configured")
	}
	if s.scanner.DashboardEndpoint == "" {
		return fmt.Errorf("dashboard endpoint not configured")
	}

	httpClient := s.unscopedRefreshHTTPClient()
	dc := dashboard.NewClient(httpClient, s.scanner.DashboardEndpoint)
	user, err := dc.FetchCurrentUser(ctx)
	if err != nil {
		return fmt.Errorf("fetching user profile: %w", err)
	}

	orgs := make([]auth.CachedOrganization, len(user.Organizations))
	for i, o := range user.Organizations {
		orgs[i] = auth.CachedOrganization{ID: o.ID, Name: o.Name, Slug: o.Slug}
	}

	authCfg := newAuthConfig()
	existing, err := authCfg.LoadUserCache()
	if err != nil {
		return fmt.Errorf("loading existing user cache: %w", err)
	}

	uc := &auth.UserCache{
		ID:            user.ID,
		Name:          user.Name,
		Email:         user.Email,
		Organizations: orgs,
	}
	if existing != nil {
		uc.SelectedOrgID = existing.SelectedOrgID
	}

	if err := authCfg.SaveUserCache(uc); err != nil {
		return fmt.Errorf("saving user cache: %w", err)
	}
	slog.Info("login: user cache refreshed", "orgs", len(orgs))
	return nil
}

func (s *Server) unscopedRefreshHTTPClient() *http.Client {
	if s.tokenSource != nil && s.tokenSource.Valid() {
		httpClient, _ := api.NewHTTPClient(s.tokenSource)
		return httpClient
	}
	if s.scanner != nil && s.scanner.HTTPClient != nil {
		return s.scanner.HTTPClient
	}
	return http.DefaultClient
}

func cachedOrgIDExists(orgs []auth.CachedOrganization, id string) bool {
	for _, o := range orgs {
		if o.ID == id {
			return true
		}
	}
	return false
}

// newAuthConfig returns an auth.Config with default paths populated.
// It honours INFRACOST_CLI_USER_CACHE_PATH when set, which also makes the
// function testable without touching the real user cache.
func newAuthConfig() *auth.Config {
	cfg := &auth.Config{
		Environment: environment.Production,
	}
	if p := os.Getenv("INFRACOST_CLI_USER_CACHE_PATH"); p != "" {
		cfg.UserCachePath = p
	}
	cfg.Process()
	return cfg
}
