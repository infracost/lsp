package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/infracost/cli/pkg/auth"
	"github.com/infracost/cli/pkg/environment"
)

// OrgEntry represents a single organization.
type OrgEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// OrgInfo is the response type for infracost/orgs and infracost/selectOrg.
type OrgInfo struct {
	Organizations []OrgEntry `json:"organizations"`
	SelectedOrgID string     `json:"selectedOrgId"`
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
		return &OrgInfo{}, nil
	}

	orgs := make([]OrgEntry, len(uc.Organizations))
	for i, o := range uc.Organizations {
		orgs[i] = OrgEntry{ID: o.ID, Name: o.Name, Slug: o.Slug}
	}

	selectedID := uc.SelectedOrgID

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
					break
				}
			}
		}
	}

	if selectedID == "" && len(orgs) > 0 {
		selectedID = orgs[0].ID
	}

	return &OrgInfo{
		Organizations: orgs,
		SelectedOrgID: selectedID,
	}, nil
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
