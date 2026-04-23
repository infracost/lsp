package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/infracost/cli/pkg/auth"
	"github.com/infracost/cli/pkg/environment"
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

// HandleOrgs returns the list of organizations the user belongs to and the
// currently active org, resolved in priority order:
//  1. .infracost/org in the workspace root (repo-scoped)
//  2. selectedOrgId in user.json (global)
//  3. first org in the list
func (s *Server) HandleOrgs(_ context.Context, _ json.RawMessage) (any, error) {
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
func (s *Server) refreshUserCache(ctx context.Context) {
	dc := dashboard.NewClient(s.scanner.HTTPClient, s.scanner.DashboardEndpoint)
	user, err := dc.FetchCurrentUser(ctx)
	if err != nil {
		slog.Warn("login: failed to fetch user profile", "error", err)
		return
	}

	orgs := make([]auth.CachedOrganization, len(user.Organizations))
	for i, o := range user.Organizations {
		orgs[i] = auth.CachedOrganization{ID: o.ID, Name: o.Name, Slug: o.Slug}
	}

	authCfg := newAuthConfig()
	existing, err := authCfg.LoadUserCache()
	if err != nil {
		slog.Warn("login: failed to load existing user cache", "error", err)
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
		slog.Warn("login: failed to save user cache", "error", err)
		return
	}
	slog.Info("login: user cache refreshed", "orgs", len(orgs))
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
