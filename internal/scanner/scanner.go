package scanner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	cliplugins "github.com/infracost/cli/pkg/plugins"
	repoconfig "github.com/infracost/config"
	"github.com/infracost/go-proto/pkg/address"
	"github.com/infracost/go-proto/pkg/diagnostic"
	goprotoevent "github.com/infracost/go-proto/pkg/event"
	providerconv "github.com/infracost/go-proto/pkg/providers"
	"github.com/infracost/go-proto/pkg/rat"
	treeresource "github.com/infracost/go-proto/pkg/tree/resource"
	parserapi "github.com/infracost/proto/gen/go/infracost/parser/api"
	"github.com/infracost/proto/gen/go/infracost/parser/event"
	"github.com/infracost/proto/gen/go/infracost/parser/options"
	pluginpb "github.com/infracost/proto/gen/go/infracost/plugin"
	"github.com/infracost/proto/gen/go/infracost/provider"
	treepb "github.com/infracost/proto/gen/go/infracost/tree"
	"github.com/infracost/proto/gen/go/infracost/usage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/infracost/lsp/internal/api"
	"github.com/infracost/lsp/internal/dashboard"
	"github.com/infracost/lsp/internal/vcs"
)

// Scanner orchestrates parsing and pricing of IaC projects.
type Scanner struct {
	// Plugins owns the full parser/provider plugin lifecycle (S3 download,
	// connect, client caching, and reconnect-on-error), delegated to the CLI.
	Plugins           *cliplugins.Config
	Currency          string
	PricingEndpoint   string
	DashboardEndpoint string
	TokenSource       *api.TokenSource
	HTTPClient        *http.Client
	OnOrgID           func(string)
	OnLog             func(level, message string, fields map[string]any)

	tagPolicies        []*event.TagPolicy
	finopsPolicies     []*event.FinopsPolicySettings
	guardrails         []*event.Guardrail
	productionFilters  []*event.ProductionFilter
	usageDefaults      *event.UsageDefaults
	repositoryName     string
	configTemplate     string
	currencyMu         sync.RWMutex
	exchangeRateMu     sync.Mutex
	exchangeRates      map[string]*rat.Rat
	runParamsMu        sync.RWMutex
	runParamsOrgID     string
	runParamsFetchedAt time.Time
	runParamsTTL       time.Duration

	policyDetailMu    sync.RWMutex
	policyDetailCache map[string]dashboard.PolicyDetail
}

// Init initializes internal state. Must be called before first use.
func (s *Scanner) Init() {
	s.policyDetailCache = make(map[string]dashboard.PolicyDetail)
}

func (s *Scanner) GetConfigTemplate() string {
	return s.configTemplate
}

// accessToken returns a valid access token from the token source.
func (s *Scanner) accessToken() (string, error) {
	if s.TokenSource == nil {
		return "", fmt.Errorf("no token source configured")
	}

	tok, err := s.TokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("getting token: %w", err)
	}

	slog.Debug("auth: got token from source", "expiry", tok.Expiry, "valid", tok.Valid())
	return tok.AccessToken, nil
}

// HasTokenSource reports whether a token source is configured.
func (s *Scanner) HasTokenSource() bool {
	return s.TokenSource != nil && s.TokenSource.Valid()
}

// CurrencyOrDefault returns the scanner currency, falling back to USD.
func (s *Scanner) CurrencyOrDefault() string {
	s.currencyMu.RLock()
	currency := s.Currency
	s.currencyMu.RUnlock()

	return CurrencyOrDefault(currency)
}

// SetCurrency updates the scanner currency. It returns true when the value changed.
func (s *Scanner) SetCurrency(currency string) bool {
	currency = CurrencyOrDefault(currency)

	s.currencyMu.Lock()
	if s.Currency == currency {
		s.currencyMu.Unlock()
		return false
	}
	s.Currency = currency
	s.currencyMu.Unlock()

	s.exchangeRateMu.Lock()
	s.exchangeRates = nil
	s.exchangeRateMu.Unlock()

	if s.Plugins != nil {
		s.Plugins.Providers.Close()
	}
	return true
}

// SetRunParamsTTL sets how long fetchRunParams results are cached.
func (s *Scanner) SetRunParamsTTL(d time.Duration) {
	s.runParamsMu.Lock()
	s.runParamsTTL = d
	s.runParamsMu.Unlock()
	slog.Info("scanner: runParams cache TTL set", "ttl", d)
}

// FetchRunParams queries the dashboard API for run parameters (org ID, tag policies, etc.)
// and stores parsed tag policies on the scanner. Returns org ID (empty on failure, non-fatal).
func (s *Scanner) FetchRunParams(ctx context.Context, rootDir string) string {
	if !s.TokenSource.Valid() {
		return ""
	}

	s.runParamsMu.RLock()
	if s.runParamsTTL > 0 && !s.runParamsFetchedAt.IsZero() && time.Since(s.runParamsFetchedAt) < s.runParamsTTL {
		orgID := s.runParamsOrgID
		age := time.Since(s.runParamsFetchedAt)
		s.runParamsMu.RUnlock()
		slog.Debug("fetchRunParams: using cached result", "org_id", orgID, "age", age)
		return orgID
	}
	s.runParamsMu.RUnlock()

	repoURL := vcs.GetRemoteURL(rootDir)
	branch := vcs.GetCurrentBranch(rootDir)

	client := dashboard.NewClient(s.HTTPClient, s.DashboardEndpoint)
	params, err := client.RunParameters(ctx, "", repoURL, branch)
	if err != nil {
		slog.Warn("fetchRunParams: failed to get run parameters", "error", err)
		return ""
	}

	tagPolicies := make([]*event.TagPolicy, 0, len(params.TagPolicies))
	for i, raw := range params.TagPolicies {
		slog.Debug("fetchRunParams: raw tag policy", "index", i, "json", string(raw))
		var tp event.TagPolicy
		if err := protojson.Unmarshal(raw, &tp); err != nil {
			slog.Warn("fetchRunParams: failed to unmarshal tag policy", "error", err)
			continue
		}
		tagPolicies = append(tagPolicies, &tp)
	}

	finopsPolicies := make([]*event.FinopsPolicySettings, 0, len(params.FinopsPolicies))
	for i, raw := range params.FinopsPolicies {
		slog.Debug("fetchRunParams: raw finops policy", "index", i, "json", string(raw))
		var fp event.FinopsPolicySettings
		if err := protojson.Unmarshal(raw, &fp); err != nil {
			slog.Warn("fetchRunParams: failed to unmarshal finops policy", "error", err)
			continue
		}
		finopsPolicies = append(finopsPolicies, &fp)
	}

	guardrails := make([]*event.Guardrail, 0, len(params.Guardrails))
	for i, raw := range params.Guardrails {
		slog.Debug("fetchRunParams: raw guardrail", "index", i, "json", string(raw))
		var g event.Guardrail
		if err := protojson.Unmarshal(raw, &g); err != nil {
			slog.Warn("fetchRunParams: failed to unmarshal guardrail", "error", err)
			continue
		}
		guardrails = append(guardrails, &g)
	}
	guardrails = dedupeGuardrails(guardrails)

	productionFilters := make([]*event.ProductionFilter, 0, len(params.ProductionFilters))
	for _, raw := range params.ProductionFilters {
		var pf event.ProductionFilter
		if err := protojson.Unmarshal(raw, &pf); err != nil {
			slog.Warn("fetchRunParams: failed to unmarshal production filter", "error", err)
			continue
		}
		productionFilters = append(productionFilters, &pf)
	}

	s.configTemplate = params.ConfigTemplate

	s.usageDefaults = nil
	var usageDefaults *event.UsageDefaults
	if len(params.UsageDefaults) > 0 {
		var ud event.UsageDefaults
		if err := protojson.Unmarshal(params.UsageDefaults, &ud); err != nil {
			slog.Warn("fetchRunParams: failed to unmarshal usage defaults", "error", err)
		} else {
			usageDefaults = &ud
		}
	}

	s.runParamsMu.Lock()
	s.tagPolicies = tagPolicies
	s.finopsPolicies = finopsPolicies
	s.guardrails = guardrails
	s.productionFilters = productionFilters
	s.repositoryName = params.RepositoryName
	s.usageDefaults = usageDefaults
	s.runParamsOrgID = params.OrganizationID
	s.runParamsFetchedAt = time.Now()
	s.runParamsMu.Unlock()

	if s.OnOrgID != nil {
		s.OnOrgID(params.OrganizationID)
	}

	slog.Debug("fetchRunParams: resolved", "org_id", params.OrganizationID, "tag_policies", len(tagPolicies))
	return params.OrganizationID
}

type runParamsSnapshot struct {
	usageDefaults     *event.UsageDefaults
	productionFilters []*event.ProductionFilter
	repositoryName    string
	finopsPolicies    []*event.FinopsPolicySettings
	tagPolicies       []*event.TagPolicy
	guardrails        []*event.Guardrail
}

func (s *Scanner) runParamsSnapshot() runParamsSnapshot {
	s.runParamsMu.RLock()
	defer s.runParamsMu.RUnlock()
	return runParamsSnapshot{
		usageDefaults:     s.usageDefaults,
		productionFilters: append([]*event.ProductionFilter(nil), s.productionFilters...),
		repositoryName:    s.repositoryName,
		finopsPolicies:    append([]*event.FinopsPolicySettings(nil), s.finopsPolicies...),
		tagPolicies:       append([]*event.TagPolicy(nil), s.tagPolicies...),
		guardrails:        append([]*event.Guardrail(nil), s.guardrails...),
	}
}

// EvaluateGuardrails evaluates the cached guardrail configs against the
// provided per-project costs and returns one result per guardrail.
func (s *Scanner) EvaluateGuardrails(projects []goprotoevent.ProjectCostInfo) []goprotoevent.GuardrailResult {
	guardrails := s.runParamsSnapshot().guardrails
	if len(guardrails) == 0 {
		return nil
	}

	headTotal := rat.Zero
	pastTotal := rat.Zero
	for _, p := range projects {
		if p.TotalMonthlyCost != nil {
			headTotal = headTotal.Add(p.TotalMonthlyCost)
		}
		if p.PastTotalMonthlyCost != nil {
			pastTotal = pastTotal.Add(p.PastTotalMonthlyCost)
		}
	}

	return goprotoevent.Guardrails(s.guardrailsForCurrency(guardrails)).Evaluate(headTotal, pastTotal, projects)
}

func (s *Scanner) guardrailsForCurrency(guardrails []*event.Guardrail) []*event.Guardrail {
	baseGuardrails := dedupeGuardrails(guardrails)
	currency := s.CurrencyOrDefault()
	if currency == "USD" {
		return baseGuardrails
	}

	rate := s.cachedExchangeRate(currency)
	if rate == nil || rate.Equals(rat.New(1)) {
		return baseGuardrails
	}

	convertedGuardrails := make([]*event.Guardrail, 0, len(baseGuardrails))
	for _, g := range baseGuardrails {
		if g == nil {
			continue
		}
		converted, ok := proto.Clone(g).(*event.Guardrail)
		if !ok {
			convertedGuardrails = append(convertedGuardrails, g)
			continue
		}
		if converted.TotalThreshold != nil {
			converted.TotalThreshold = rat.FromProto(converted.TotalThreshold).Mul(rate).Proto()
		}
		if converted.IncreaseThreshold != nil {
			converted.IncreaseThreshold = rat.FromProto(converted.IncreaseThreshold).Mul(rate).Proto()
		}
		convertedGuardrails = append(convertedGuardrails, converted)
	}
	return convertedGuardrails
}

func dedupeGuardrails(guardrails []*event.Guardrail) []*event.Guardrail {
	if len(guardrails) < 2 {
		return guardrails
	}

	seen := make(map[string]struct{}, len(guardrails))
	deduped := make([]*event.Guardrail, 0, len(guardrails))
	for _, g := range guardrails {
		if g == nil {
			continue
		}
		key := guardrailKey(g)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, g)
	}
	return deduped
}

func guardrailKey(g *event.Guardrail) string {
	if g.GetId() != "" {
		return "id:" + g.GetId()
	}
	return fmt.Sprintf("fallback:%s:%d:%s:%v:%v:%v",
		g.GetName(),
		g.GetScope(),
		g.GetMessage(),
		g.TotalThreshold,
		g.IncreaseThreshold,
		g.IncreasePercentThreshold,
	)
}

func (s *Scanner) cachedExchangeRate(currency string) *rat.Rat {
	s.exchangeRateMu.Lock()
	defer s.exchangeRateMu.Unlock()
	if s.exchangeRates == nil {
		return nil
	}
	return s.exchangeRates[CurrencyOrDefault(currency)]
}

// Close kills all persistent plugin subprocesses.
func (s *Scanner) Close() {
	if s.Plugins != nil {
		s.Plugins.Close()
	}
}

// LoadConfig loads or auto-generates an infracost config for the given directory.
func LoadConfig(dir, configTemplate string) (*repoconfig.Config, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolving path: %w", err)
	}
	return loadOrGenerateConfig(absDir, configTemplate)
}

// Scan analyzes the given directory and returns resource cost results.
func (s *Scanner) Scan(ctx context.Context, dir string) (*ScanResult, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolving path: %w", err)
	}

	cfg, err := loadOrGenerateConfig(absDir, s.configTemplate)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	return s.ScanAll(ctx, absDir, cfg)
}

// loadRepoUsage loads repo-level usage: API defaults merged with the repo's usage YAML file.
func (s *Scanner) loadRepoUsage(rootDir string, cfg *repoconfig.Config, usageDefaults *event.UsageDefaults) *usage.Usage {
	repoUsage := loadUsageDefaults(usageDefaults, "")
	if cfg.UsageFilePath != "" {
		usagePath := filepath.Join(rootDir, cfg.UsageFilePath)
		if stat, err := os.Stat(usagePath); err == nil && !stat.IsDir() {
			f, err := os.Open(usagePath) // #nosec G304
			if err != nil {
				slog.Warn("loadRepoUsage: failed to open usage file", "path", usagePath, "error", err)
				return repoUsage
			}
			u, err := loadUsageData(f, repoUsage)
			_ = f.Close()
			if err != nil {
				slog.Warn("loadRepoUsage: failed to load usage file", "path", usagePath, "error", err)
				return repoUsage
			}
			repoUsage = u
		}
	}
	return repoUsage
}

// ScanAll scans all projects in the given config.
func (s *Scanner) ScanAll(ctx context.Context, rootDir string, cfg *repoconfig.Config) (*ScanResult, error) {
	result := &ScanResult{}

	orgID := s.FetchRunParams(ctx, rootDir)
	params := s.runParamsSnapshot()
	slog.Debug("scanAll: starting", "projects", len(cfg.Projects), "currency", cfg.Currency, "org_id", orgID)

	repoUsage := s.loadRepoUsage(rootDir, cfg, params.usageDefaults)
	branchName := vcs.GetCurrentBranch(rootDir)

	for _, project := range cfg.Projects {
		r, err := s.scanProject(ctx, rootDir, cfg, project, orgID, repoUsage, branchName, params)
		if err != nil {
			slog.Error("scanAll: project failed", "name", project.Name, "error", err)
			result.Errors = append(result.Errors, fmt.Sprintf("project %s: %v", project.Name, err))
			continue
		}

		result.Resources = append(result.Resources, r.Resources...)
		result.ModuleCosts = append(result.ModuleCosts, r.ModuleCosts...)
		result.Violations = append(result.Violations, r.Violations...)
		result.TagViolations = append(result.TagViolations, r.TagViolations...)
		result.Errors = append(result.Errors, r.Errors...)
	}

	return result, nil
}

// ScanProject scans a single project and returns its results.
func (s *Scanner) ScanProject(ctx context.Context, rootDir string, cfg *repoconfig.Config, project *repoconfig.Project) (*ScanResult, error) {
	orgID := s.FetchRunParams(ctx, rootDir)
	params := s.runParamsSnapshot()
	repoUsage := s.loadRepoUsage(rootDir, cfg, params.usageDefaults)
	branchName := vcs.GetCurrentBranch(rootDir)
	return s.scanProject(ctx, rootDir, cfg, project, orgID, repoUsage, branchName, params)
}

func (s *Scanner) scanProject(ctx context.Context, rootDir string, cfg *repoconfig.Config, project *repoconfig.Project, orgID string, repoUsage *usage.Usage, branchName string, params runParamsSnapshot) (*ScanResult, error) {
	projectPath := filepath.Clean(filepath.Join(rootDir, project.Path))
	slog.Debug("scanProject: starting", "name", project.Name, "path", projectPath)

	projectType := finalProjectType(project.Type, projectPath)

	parseStart := time.Now()
	parseResp, err := s.parse(ctx, projectPath, cfg, project, rootDir, projectType, params.finopsPolicies)
	parseDuration := time.Since(parseStart)

	result := &ScanResult{}

	// Check diagnostics even on error — the parser may return both.
	if parseResp != nil && parseResp.Diagnostics != nil {
		diags := diagnostic.FromProto(parseResp.Diagnostics)
		for _, d := range diags.Unwrap() {
			if d.Critical {
				slog.Warn("scanProject: critical diagnostic", "message", d.String())
				result.Errors = append(result.Errors, d.String())
			} else {
				slog.Debug("scanProject: diagnostic", "message", d.String(), "warning", d.Warning)
			}
		}
		if diags.Critical().Len() > 0 {
			slog.Warn("scanProject: critical diagnostics, stopping", "project", project.Name, "count", diags.Critical().Len())
			return result, nil
		}
	}

	if err != nil {
		return result, fmt.Errorf("parsing: %w", err)
	}
	slog.Debug("scanProject: parse complete", "path", projectPath, "elapsed", parseDuration)

	if parseResp == nil || parseResp.Tree == nil {
		return nil, fmt.Errorf("parser returned no tree")
	}
	tree := parseResp.Tree

	requiredProviders := GetRequiredProvidersFromTree(tree)
	if len(requiredProviders) == 0 && projectType == repoconfig.ProjectTypeCloudFormation {
		requiredProviders = []provider.Provider{provider.Provider_PROVIDER_AWS}
	}

	srcLocs := make(map[string]sourceLocation)
	modLocs := make(map[string]sourceLocation)
	collectTreeSourceLocations(tree, srcLocs, modLocs)
	slog.Debug("scanProject: source locations", "count", len(srcLocs), "module_locations", len(modLocs), "providers", requiredProviders)

	currency := s.CurrencyOrDefault()

	pricingEndpoint := s.PricingEndpoint
	if pricingEndpoint == "" {
		pricingEndpoint = "https://pricing.api.infracost.io"
	}

	token, err := s.accessToken()
	if err != nil {
		return nil, fmt.Errorf("authentication: %w", err)
	}

	isProduction := evaluateProductionFilters(params.productionFilters, params.repositoryName, branchName, project.Name)

	// Load project-level usage (overlay on top of repo-level).
	projectUsage := repoUsage
	if project.UsageFile != "" && project.UsageFile != cfg.UsageFilePath {
		usagePath := filepath.Join(rootDir, project.UsageFile)
		if stat, err := os.Stat(usagePath); err == nil && !stat.IsDir() {
			f, err := os.Open(usagePath) // #nosec G304
			if err != nil {
				slog.Warn("scanProject: failed to open project usage file", "path", usagePath, "error", err)
			} else {
				usageDefaults := loadUsageDefaults(params.usageDefaults, project.Name)
				u, err := loadUsageData(f, usageDefaults)
				_ = f.Close()
				if err == nil {
					projectUsage = u
				} else {
					slog.Warn("scanProject: failed to load project usage file", "path", usagePath, "error", err)
				}
			}
		}
	}

	input := &provider.TreeInput{
		Tree:         tree,
		AbsolutePath: projectPath,
		Usage:        projectUsage,
		ProjectInfo: &provider.ProjectInfo{
			Name:         project.Name,
			BranchName:   branchName,
			Workspace:    project.Terraform.Workspace,
			IsProduction: isProduction,
		},
		Features: &provider.Features{
			EnablePriceLookups:         true,
			EnableRecommendations:      true,
			EnableFinopsPolicies:       true,
			EnableEnvironmentalMetrics: true,
		},
		FinopsPolicyConfig: &provider.FinopsPolicyConfiguration{
			Policies: params.finopsPolicies,
		},
		Settings: &provider.Settings{
			Currency:      currency,
			UseDiskCaches: useDiskCaches(currency),
		},
		Infracost: &provider.Infracost{
			ApiKey:             token,
			PricingApiEndpoint: pricingEndpoint,
			OrgId:              &orgID,
		},
	}

	allResources := make([]*provider.Resource, 0, len(tree.GetUnsupportedResources()))
	var allFinops []*provider.FinopsPolicyResult

	// Parser-flagged unsupported resources never reach a provider, but tag
	// policies still evaluate against them, so seed the result set here.
	for _, res := range tree.GetUnsupportedResources() {
		pr := treeresource.ProtoToProviderResource(res)
		if pr == nil {
			continue
		}
		if pr.IsFree {
			pr.IsSupported = true
		}
		allResources = append(allResources, pr)
	}

	for _, rp := range requiredProviders {
		rs, ps, provErrs := s.processProvider(ctx, rp, input)
		slog.Debug("scanProject: provider complete",
			"provider", rp.String(),
			"resources", len(rs),
			"finops_results", len(ps),
			"errors", len(provErrs),
		)
		allResources = append(allResources, rs...)
		allFinops = append(allFinops, ps...)
		result.Errors = append(result.Errors, provErrs...)
	}

	exchangeRate, err := s.currencyExchangeRate(ctx, currency, token)
	if err != nil {
		msg := fmt.Sprintf("currency exchange rate for %s: %v", currency, err)
		exchangeRate = s.fallbackExchangeRate(currency)
		s.logWarn("currencyExchangeRate: using USD fallback rate", map[string]any{"currency": currency, "rate": exchangeRate.String(), "error": err.Error()})
		result.Errors = append(result.Errors, msg)
	}
	result.Resources = make([]ResourceResult, 0, len(allResources))
	for _, r := range allResources {
		monthlyCost, components := resourceCost(r, exchangeRate)

		rr := ResourceResult{
			Name:           r.Name,
			Type:           r.Type,
			MonthlyCost:    monthlyCost,
			CostComponents: components,
			IsSupported:    r.IsSupported,
			IsFree:         r.IsFree,
		}

		if r.Metadata != nil && r.Metadata.Filename != "" {
			rr.Filename = resolveFilename(rootDir, r.Metadata.Filename)
			rr.StartLine = r.Metadata.StartLine
			rr.EndLine = r.Metadata.EndLine
		} else if loc, ok := srcLocs[r.Name]; ok {
			rr.Filename = resolveFilename(rootDir, loc.Filename)
			rr.StartLine = loc.StartLine
			rr.EndLine = loc.EndLine
		}

		slog.Debug("scanProject: resource",
			"name", rr.Name,
			"file", rr.Filename,
			"cost", FormatCost(rr.MonthlyCost),
		)

		result.Resources = append(result.Resources, rr)
	}

	// Aggregate costs per top-level module.
	type modAgg struct {
		cost  *rat.Rat
		count int
	}
	modCosts := make(map[string]*modAgg)
	for _, r := range result.Resources {
		prefix := topLevelModulePrefix(r.Name)
		if prefix == "" {
			continue
		}
		agg, ok := modCosts[prefix]
		if !ok {
			agg = &modAgg{cost: rat.Zero}
			modCosts[prefix] = agg
		}
		agg.cost = agg.cost.Add(r.MonthlyCost)
		agg.count++
	}

	slog.Debug("scanProject: module aggregation", "module_prefixes", len(modCosts), "module_locations", len(modLocs))
	for addr, agg := range modCosts {
		loc, ok := modLocs[addr]
		if !ok {
			slog.Debug("scanProject: no location for module", "addr", addr)
			continue
		}
		slog.Debug("scanProject: module cost", "addr", addr, "cost", FormatCost(agg.cost), "resources", agg.count, "file", loc.Filename, "line", loc.StartLine)
		result.ModuleCosts = append(result.ModuleCosts, ModuleCost{
			Name:          addr,
			Filename:      resolveFilename(rootDir, loc.Filename),
			StartLine:     loc.StartLine,
			EndLine:       loc.EndLine,
			MonthlyCost:   agg.cost,
			ResourceCount: agg.count,
		})
	}

	result.Violations = convertFinopsViolations(allFinops, allResources, rootDir, srcLocs, currency, exchangeRate)

	if len(result.Violations) > 0 && s.HasTokenSource() && orgID != "" {
		s.attachPolicyDetails(ctx, orgID, result.Violations, currency)
	}

	if len(params.tagPolicies) > 0 {
		tagResults := goprotoevent.TagPolicies(params.tagPolicies).EvaluateAgainstResources(allResources, input.ProjectInfo)
		result.TagViolations = convertTagViolations(tagResults, allResources, rootDir, srcLocs)
		slog.Debug("scanProject: tag policy evaluation", "results", len(tagResults), "violations", len(result.TagViolations))
	}

	return result, nil
}

func (s *Scanner) attachPolicyDetails(ctx context.Context, orgID string, violations []FinopsViolation, currency string) {
	var uncached []string
	s.policyDetailMu.RLock()
	for _, v := range violations {
		if v.PolicySlug == "" {
			continue
		}
		if _, ok := s.policyDetailCache[v.PolicySlug]; !ok {
			uncached = append(uncached, v.PolicySlug)
		}
	}
	s.policyDetailMu.RUnlock()

	if len(uncached) > 0 {
		client := dashboard.NewClient(s.HTTPClient, s.DashboardEndpoint)
		details, err := client.PolicyDetails(ctx, orgID, uncached)
		if err != nil {
			slog.Warn("attachPolicyDetails: failed to fetch policy details", "error", err)
			return
		}
		s.policyDetailMu.Lock()
		for slug, pd := range details {
			s.policyDetailCache[slug] = pd
		}
		s.policyDetailMu.Unlock()
	}

	s.policyDetailMu.RLock()
	for i := range violations {
		if pd, ok := s.policyDetailCache[violations[i].PolicySlug]; ok {
			pd := pd // copy for pointer
			violations[i].PolicyDetail = &pd
			violations[i].Markdown = buildViolationMarkdownCurrency(violations[i], currency)
		}
	}
	totalCached := len(s.policyDetailCache)
	s.policyDetailMu.RUnlock()

	slog.Debug("attachPolicyDetails: attached", "uncached", len(uncached), "total_cached", totalCached)
}

func (s *Scanner) processProvider(ctx context.Context, prov provider.Provider, input *provider.TreeInput) ([]*provider.Resource, []*provider.FinopsPolicyResult, []string) {
	name := providerconv.FromProto(prov)

	loader := s.providerLoader(prov)
	if loader == nil {
		return nil, nil, []string{fmt.Sprintf("unknown provider: %s", name)}
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// ProcessTreeInput owns plugin loading, response caching, and
	// reconnect-on-error (the cached client is evicted when a call fails).
	processStart := time.Now()
	rs, ps, err := s.Plugins.Providers.ProcessTreeInput(ctx, prov, input, loader, s.hclogLevel())
	processDuration := time.Since(processStart)
	if err != nil {
		slog.Error("processProvider: Process failed", "provider", name, "error", err, "elapsed", processDuration)
		return nil, nil, []string{fmt.Sprintf("%s provider error: %v", name, err)}
	}

	slog.Debug("processProvider: complete", "provider", name, "resources", len(rs), "finops", len(ps), "elapsed", processDuration)
	return rs, ps, nil
}

// providerLoader returns the cliplugins loader for the given provider, or nil
// if the provider is not one we support.
func (s *Scanner) providerLoader(prov provider.Provider) func(hclog.Level) (pluginpb.ProviderServiceClient, func(), error) {
	switch prov {
	case provider.Provider_PROVIDER_AWS:
		return s.Plugins.Providers.LoadAWS
	case provider.Provider_PROVIDER_GOOGLE:
		return s.Plugins.Providers.LoadGoogle
	case provider.Provider_PROVIDER_AZURERM:
		return s.Plugins.Providers.LoadAzurerm
	default:
		return nil
	}
}

// hclogLevel maps the configured slog level onto the hclog level the CLI
// plugin loaders expect.
func (s *Scanner) hclogLevel() hclog.Level {
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		return hclog.Debug
	}
	return hclog.Info
}

func (s *Scanner) parse(ctx context.Context, path string, cfg *repoconfig.Config, project *repoconfig.Project, rootDir string, projectType repoconfig.ProjectType, finopsPolicies []*event.FinopsPolicySettings) (*pluginpb.ParseResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	pp, err := s.Plugins.ParserPluginForProject(ctx, string(projectType))
	if err != nil {
		// EnsurePlugins caches its result behind a sync.Once, so a transient
		// download failure would otherwise stick for the life of the server.
		// Reset so the next scan retries.
		s.Plugins.ResetParserPlugins()
		return nil, fmt.Errorf("loading parser plugin for project type %q: %w", projectType, err)
	}

	genericOpts := buildGenericOptions(project, rootDir)
	rawOpts, rawFormat, err := buildIaCOptions(cfg, project, projectType, finopsPolicies)
	if err != nil {
		return nil, fmt.Errorf("building parser plugin options: %w", err)
	}

	slog.Debug("parse: calling Parse", "path", path, "project_name", project.Name, "project_type", projectType)

	req := &pluginpb.ParseRequest{
		Path:             path,
		GenericOptions:   genericOpts,
		RawOptions:       rawOpts,
		RawOptionsFormat: rawFormat,
	}
	resp, err := pp.Parse(ctx, req)
	if err != nil && isTransportError(err) {
		slog.Warn("parse: transport error, reconnecting", "error", err)
		s.Plugins.ResetParserPlugins()
		pp, err = s.Plugins.ParserPluginForProject(ctx, string(projectType))
		if err != nil {
			return nil, fmt.Errorf("reconnecting parser plugin: %w", err)
		}
		resp, err = pp.Parse(ctx, req)
	}
	if err != nil {
		slog.Error("parse: Parse failed", "error", err)
		s.Plugins.ResetParserPlugins()
		return resp, err
	}

	slog.Debug("parse: complete", "has_tree", resp != nil && resp.Tree != nil)
	return resp, nil
}

// finalProjectType resolves an unknown or CDK project type the same way the CLI
// scanner does, so the parser plugin can be matched by project type.
func finalProjectType(projectType repoconfig.ProjectType, absoluteProjectPath string) repoconfig.ProjectType {
	if projectType == repoconfig.ProjectTypeUnknown {
		if stat, err := os.Stat(filepath.Join(absoluteProjectPath, "terragrunt.hcl")); err == nil && !stat.IsDir() {
			return repoconfig.ProjectTypeTerragrunt
		}
		if stat, err := os.Stat(filepath.Join(absoluteProjectPath, "terragrunt.hcl.json")); err == nil && !stat.IsDir() {
			return repoconfig.ProjectTypeTerragrunt
		}
		return repoconfig.ProjectTypeTerraform
	}
	if strings.HasPrefix(string(projectType), "cdk_") {
		return repoconfig.ProjectTypeCloudFormation
	}
	return projectType
}

func buildGenericOptions(project *repoconfig.Project, rootDir string) *options.GenericOptions {
	cacheDir := filepath.Join(os.TempDir(), ".infracost", "cache")
	_ = os.MkdirAll(cacheDir, 0o700)

	return &options.GenericOptions{
		ProjectName:        project.Name,
		EnvironmentName:    project.EnvName,
		RepoDirectory:      rootDir,
		TemporaryDirectory: os.TempDir(),
		CacheDirectory:     cacheDir,
		WorkingDirectory:   rootDir,
	}
}

func buildIaCOptions(cfg *repoconfig.Config, project *repoconfig.Project, projectType repoconfig.ProjectType, finopsPolicies []*event.FinopsPolicySettings) ([]byte, string, error) {
	switch projectType {
	case repoconfig.ProjectTypeTerraform, repoconfig.ProjectTypeTerragrunt, repoconfig.ProjectTypeCiscoStacks:
		data, err := json.Marshal(buildTerraformPluginOptions(cfg, project, namingPolicyAttributeRequirements(finopsPolicies)))
		return data, "application/json", err
	case repoconfig.ProjectTypeCloudFormation:
		data, err := json.Marshal(buildCloudFormationPluginOptions(project))
		return data, "application/json", err
	default:
		return nil, "", nil
	}
}

type terraformPluginOptions struct {
	RegexSourceMap              map[string]string            `json:"regexSourceMap,omitempty"`
	Env                         map[string]string            `json:"env,omitempty"`
	Vars                        map[string]any               `json:"vars,omitempty"`
	Workspace                   string                       `json:"workspace,omitempty"`
	TfVarsFiles                 []string                     `json:"tfVarsFiles,omitempty"`
	TerraformCloudConfiguration *terraformCloudConfiguration `json:"terraformCloudConfiguration,omitempty"`
	RequiredAttributes          map[string][]string          `json:"requiredAttributes,omitempty"`
}

type terraformCloudConfiguration struct {
	Organization string `json:"organization"`
	Workspace    string `json:"workspace"`
	Hostname     string `json:"hostname"`
}

type cloudFormationPluginOptions struct {
	InputParameters map[string]any `json:"inputParameters"`
	AWSContext      *awsContext    `json:"awsContext"`
}

type awsContext struct {
	Region    string `json:"region"`
	AccountID string `json:"accountId"`
	StackID   string `json:"stackId"`
	StackName string `json:"stackName"`
}

func buildTerraformPluginOptions(cfg *repoconfig.Config, project *repoconfig.Project, requiredAttributes []*parserapi.AttributeRequirement) *terraformPluginOptions {
	regexSourceMap := make(map[string]string, len(cfg.Terraform.SourceMap))
	for _, source := range cfg.Terraform.SourceMap {
		regexSourceMap[source.Match] = source.Replace
	}

	var cloudConfig *terraformCloudConfiguration
	if project.Terraform.Cloud.Org != "" || project.Terraform.Cloud.Workspace != "" || project.Terraform.Cloud.Host != "" {
		cloudConfig = &terraformCloudConfiguration{
			Organization: project.Terraform.Cloud.Org,
			Workspace:    project.Terraform.Cloud.Workspace,
			Hostname:     project.Terraform.Cloud.Host,
		}
	}

	tfRequiredAttributes := make(map[string][]string, len(requiredAttributes))
	for _, attr := range requiredAttributes {
		tfRequiredAttributes[attr.ResourceType] = attr.Attributes
	}

	return &terraformPluginOptions{
		RegexSourceMap:              regexSourceMap,
		Env:                         project.Env,
		Vars:                        project.Terraform.Vars,
		Workspace:                   project.Terraform.Workspace,
		TfVarsFiles:                 project.Terraform.VarFiles,
		TerraformCloudConfiguration: cloudConfig,
		RequiredAttributes:          tfRequiredAttributes,
	}
}

func buildCloudFormationPluginOptions(project *repoconfig.Project) *cloudFormationPluginOptions {
	var awsCtx *awsContext
	if project.AWS.Region != "" || project.AWS.AccountID != "" || project.AWS.StackID != "" || project.AWS.StackName != "" {
		awsCtx = &awsContext{
			Region:    project.AWS.Region,
			AccountID: project.AWS.AccountID,
			StackID:   project.AWS.StackID,
			StackName: project.AWS.StackName,
		}
	}

	return &cloudFormationPluginOptions{
		InputParameters: nil,
		AWSContext:      awsCtx,
	}
}

type namingValidationSettings struct {
	AttributeValidationRules []string `json:"attributeValidationRules"`
}

// namingPolicyAttributeRequirements derives the resource attributes the parser
// must surface so naming-validation finops policies can be evaluated.
func namingPolicyAttributeRequirements(policies []*event.FinopsPolicySettings) []*parserapi.AttributeRequirement {
	grouped := make(map[string]map[string]struct{})
	for _, policy := range policies {
		if len(policy.GetSettings()) <= 2 {
			continue
		}

		var settings namingValidationSettings
		if err := json.Unmarshal([]byte(policy.GetSettings()), &settings); err != nil {
			continue
		}

		for _, rule := range settings.AttributeValidationRules {
			resourceType, attribute, err := parseAttributeValidationRule(rule)
			if err != nil {
				continue
			}
			attrs, ok := grouped[resourceType]
			if !ok {
				attrs = make(map[string]struct{})
				grouped[resourceType] = attrs
			}
			attrs[attribute] = struct{}{}
		}
	}

	reqs := make([]*parserapi.AttributeRequirement, 0, len(grouped))
	for rt, attrs := range grouped {
		names := make([]string, 0, len(attrs))
		for name := range attrs {
			names = append(names, name)
		}
		sort.Strings(names)
		reqs = append(reqs, &parserapi.AttributeRequirement{ResourceType: rt, Attributes: names})
	}
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].ResourceType < reqs[j].ResourceType })
	return reqs
}

func parseAttributeValidationRule(rule string) (resourceType, attribute string, err error) {
	keyRaw, valueRaw, found := strings.Cut(rule, ":")
	if !found {
		return "", "", fmt.Errorf("invalid rule format, expected 'resource_type.attribute: /regex/': %s", rule)
	}

	key := strings.TrimSpace(keyRaw)
	value := strings.TrimSpace(valueRaw)

	resourceType, attribute, found = strings.Cut(key, ".")
	if !found {
		return "", "", fmt.Errorf("invalid attribute key, expected 'resource_type.attribute': %s", key)
	}
	resourceType = strings.TrimSpace(resourceType)
	attribute = strings.TrimSpace(attribute)
	if resourceType == "" || attribute == "" {
		return "", "", fmt.Errorf("invalid attribute key, expected non-empty resource type and attribute: %s", key)
	}

	if len(value) < 2 || value[0] != '/' || value[len(value)-1] != '/' {
		return "", "", fmt.Errorf("invalid regex literal, expected /pattern/: %s", value)
	}
	if _, err := regexp.Compile(value[1 : len(value)-1]); err != nil {
		return "", "", err
	}

	return resourceType, attribute, nil
}

// GetRequiredProvidersFromTree extracts the cloud providers required by a
// parsed tree, mirroring the CLI scanner.
func GetRequiredProvidersFromTree(tree *treepb.Tree) []provider.Provider {
	if tree == nil {
		return nil
	}

	seen := make(map[provider.Provider]struct{})
	for raw := range tree.GetProviders() {
		if raw == "azure" {
			raw = "azurerm"
		}
		p := providerconv.ToProto(raw)
		if p == provider.Provider_PROVIDER_UNSPECIFIED {
			slog.Warn("skipping unsupported provider", "provider", raw)
			continue
		}
		seen[p] = struct{}{}
	}

	out := make([]provider.Provider, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	providerOrder := map[provider.Provider]int{
		provider.Provider_PROVIDER_AWS:     0,
		provider.Provider_PROVIDER_GOOGLE:  1,
		provider.Provider_PROVIDER_AZURERM: 2,
	}
	sort.Slice(out, func(i, j int) bool { return providerOrder[out[i]] < providerOrder[out[j]] })
	return out
}

func isTransportError(err error) bool {
	if s, ok := status.FromError(err); ok {
		switch s.Code() {
		case codes.Unavailable, codes.Internal:
			return true
		}
	}
	return false
}

// sourceLocation holds the file and line info extracted from the parser result.
type sourceLocation struct {
	Filename  string
	StartLine int64
	EndLine   int64
}

// collectTreeSourceLocations walks the parsed tree and builds a map from
// resource address (e.g. "aws_instance.my_web_app") to source location. It also
// extracts module call locations from resource CallStacks into modLocs. Source
// locations for supported resources come primarily from the provider output's
// metadata; this map is the fallback and covers unsupported resources and
// module aggregation.
func collectTreeSourceLocations(tree *treepb.Tree, out map[string]sourceLocation, modLocs map[string]sourceLocation) {
	if tree == nil {
		return
	}

	handle := func(res *treepb.Resource) {
		def := res.GetDefinition()
		if def == nil {
			return
		}

		addr := address.FromProto(def.GetAddress()).String()
		if sr := def.GetSource(); sr != nil && addr != "" {
			out[addr] = sourceLocation{
				Filename:  sr.Filename,
				StartLine: sr.StartLine,
				EndLine:   sr.EndLine,
			}
		}

		cs := def.GetCallStack()
		if cs == nil {
			return
		}
		// Frame addresses are cumulative, so each frame yields the full module
		// address up to that depth (e.g. "module.a", then "module.a.module.b").
		for _, frame := range cs.Frames {
			if frame.GetAddress() == nil || frame.GetSourceRange() == nil {
				continue
			}
			modAddr := address.FromProto(frame.GetAddress()).String()
			if modAddr == "" {
				continue
			}
			if _, ok := modLocs[modAddr]; !ok {
				modLocs[modAddr] = sourceLocation{
					Filename:  frame.SourceRange.Filename,
					StartLine: frame.SourceRange.StartLine,
					EndLine:   frame.SourceRange.EndLine,
				}
			}
		}
	}

	for _, p := range tree.GetProviders() {
		for _, svc := range p.GetServices() {
			for _, res := range svc.GetResources() {
				handle(res)
			}
		}
	}
	for _, res := range tree.GetUnsupportedResources() {
		handle(res)
	}
}

// topLevelModulePrefix extracts the top-level module prefix from a resource
// name like "module.dashboard.aws_instance.foo" → "module.dashboard".
// Returns empty string if the resource is not inside a module.
func topLevelModulePrefix(name string) string {
	if !strings.HasPrefix(name, "module.") {
		return ""
	}
	// Find the end of the first module segment: "module.<name>"
	rest := name[len("module."):]
	modName, _, ok := strings.Cut(rest, ".")
	if !ok {
		return ""
	}
	return "module." + modName
}

func convertTagViolations(results []goprotoevent.TaggingPolicyResult, resources []*provider.Resource, projectPath string, srcLocs map[string]sourceLocation) []TagViolation {
	resourceMeta := make(map[string]*provider.ResourceMetadata, len(resources))
	for _, r := range resources {
		if r.Metadata != nil {
			resourceMeta[r.Name] = r.Metadata
		}
	}

	var violations []TagViolation
	for _, tr := range results {
		for _, fr := range tr.FailingResources {
			v := TagViolation{
				PolicyID:      tr.TagPolicyID,
				PolicyName:    tr.Name,
				PolicyMessage: tr.Message,
				BlockPR:       tr.BlockPR,
				Address:       fr.Address,

				ResourceType: fr.ResourceType,
				MissingTags:  fr.MissingMandatoryTags,
			}

			for _, it := range fr.InvalidTags {
				v.InvalidTags = append(v.InvalidTags, InvalidTagResult{
					Key:         it.Key,
					Value:       it.Value,
					Suggestion:  it.Suggestion,
					Message:     it.Message,
					ValidValues: it.ValidValues,
				})
			}

			v.Message = buildTagViolationMessage(v)
			v.Markdown = buildTagViolationMarkdown(v)

			if meta, ok := resourceMeta[fr.Address]; ok && meta.Filename != "" {
				v.Filename = resolveFilename(projectPath, meta.Filename)
				v.StartLine = meta.StartLine
				v.EndLine = meta.EndLine
			} else if loc, ok := srcLocs[fr.Address]; ok {
				v.Filename = resolveFilename(projectPath, loc.Filename)
				v.StartLine = loc.StartLine
				v.EndLine = loc.EndLine
			}

			violations = append(violations, v)
		}
	}
	return violations
}

func buildTagViolationMarkdown(v TagViolation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s**\n\n", v.PolicyName)
	fmt.Fprintf(&b, "**Resource:** `%s`\n", v.Address)
	fmt.Fprintf(&b, "**Type:** `%s`\n", v.ResourceType)

	severity := "Warning"
	if v.BlockPR {
		severity = "Error (Blocking)"
	}
	fmt.Fprintf(&b, "**Severity:** %s\n\n", severity)

	if len(v.MissingTags) > 0 {
		b.WriteString("**Missing Mandatory Tags**\n\n")
		for _, tag := range v.MissingTags {
			fmt.Fprintf(&b, "- `%s`\n", tag)
		}
		b.WriteString("\n")
	}

	if len(v.InvalidTags) > 0 {
		b.WriteString("**Invalid Tags**\n\n")
		for _, tag := range v.InvalidTags {
			fmt.Fprintf(&b, "- **%s**: `%s`\n", tag.Key, tag.Value)
			if tag.Suggestion != "" {
				fmt.Fprintf(&b, "  - *Suggestion:* %s\n", tag.Suggestion)
			}
			if tag.Message != "" {
				fmt.Fprintf(&b, "  - *Note:* %s\n", tag.Message)
			}
		}
		b.WriteString("\n")
	}

	return b.String()
}

func buildTagViolationMessage(v TagViolation) string {
	var parts []string
	if len(v.MissingTags) > 0 {
		parts = append(parts, fmt.Sprintf("Missing mandatory tags: %s", strings.Join(v.MissingTags, ", ")))
	}
	for _, it := range v.InvalidTags {
		if it.Message != "" {
			parts = append(parts, it.Message)
		} else {
			parts = append(parts, fmt.Sprintf("Invalid tag `%s`: value `%s`", it.Key, it.Value))
		}
	}
	if len(parts) == 0 {
		return fmt.Sprintf("Tag policy violation: %s", v.PolicyName)
	}
	return strings.Join(parts, "; ")
}

func convertFinopsViolations(finops []*provider.FinopsPolicyResult, resources []*provider.Resource, projectPath string, srcLocs map[string]sourceLocation, currency string, exchangeRate *rat.Rat) []FinopsViolation {
	resourceMeta := make(map[string]*provider.ResourceMetadata, len(resources))
	resourcesWithHardcodedPrices := make(map[string]struct{}, len(resources))
	for _, r := range resources {
		if r.Metadata != nil {
			resourceMeta[r.Name] = r.Metadata
		}
		if resourceHasHardcodedPrice(r) {
			resourcesWithHardcodedPrices[r.Name] = struct{}{}
		}
	}

	var violations []FinopsViolation
	for _, fp := range finops {
		for _, fr := range fp.FailingResources {
			for _, issue := range fr.Issues {
				monthlySavings := rat.FromProto(issue.MonthlySavings)
				if _, ok := resourcesWithHardcodedPrices[fr.CauseAddress]; ok && exchangeRate != nil && monthlySavings != nil {
					monthlySavings = monthlySavings.Mul(exchangeRate)
				}
				v := FinopsViolation{
					PolicyID:         fp.PolicyId,
					PolicyName:       fp.PolicyName,
					PolicySlug:       fp.PolicySlug,
					BlockPullRequest: fp.BlockPullRequest,
					Message:          issue.Description,
					Address:          fr.CauseAddress,
					Attribute:        issue.Attribute,
					MonthlySavings:   monthlySavings,
				}
				if issue.SavingsDetails != nil {
					v.SavingsDetails = *issue.SavingsDetails
				}
				v.Markdown = buildViolationMarkdownCurrency(v, currency)

				if meta, ok := resourceMeta[fr.CauseAddress]; ok && meta.Filename != "" {
					v.Filename = resolveFilename(projectPath, meta.Filename)
					v.StartLine = meta.StartLine
					v.EndLine = meta.EndLine
				} else if loc, ok := srcLocs[fr.CauseAddress]; ok {
					v.Filename = resolveFilename(projectPath, loc.Filename)
					v.StartLine = loc.StartLine
					v.EndLine = loc.EndLine
				}
				violations = append(violations, v)
			}
		}
	}
	return violations
}

func resourceHasHardcodedPrice(r *provider.Resource) bool {
	if r == nil {
		return false
	}
	if r.Costs != nil {
		for _, c := range r.Costs.Components {
			if c.PriceWasHardcoded {
				return true
			}
		}
	}
	for _, child := range r.ChildResources {
		if resourceHasHardcodedPrice(child) {
			return true
		}
	}
	return false
}

func buildViolationMarkdown(v FinopsViolation) string {
	return buildViolationMarkdownCurrency(v, "USD")
}

func buildViolationMarkdownCurrency(v FinopsViolation, currency string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s**\n\n", v.PolicyName)
	fmt.Fprintf(&b, "**Policy:** %s\n", v.PolicySlug)
	fmt.Fprintf(&b, "**Resource:** `%s`\n", v.Address)
	if v.Attribute != "" {
		fmt.Fprintf(&b, "**Attribute:** `%s`\n", v.Attribute)
	}

	severity := "Warning"
	if v.BlockPullRequest {
		severity = "Error (Blocking)"
	}
	fmt.Fprintf(&b, "**Severity:** %s\n\n", severity)

	fmt.Fprintf(&b, "%s\n\n", htmlToMarkdown(v.Message))

	if v.MonthlySavings != nil && !v.MonthlySavings.IsZero() {
		fmt.Fprintf(&b, "**Potential Savings:** %s/mo\n\n", FormatCostCurrency(v.MonthlySavings, currency))
		if v.SavingsDetails != "" {
			fmt.Fprintf(&b, "%s\n\n", htmlToMarkdown(v.SavingsDetails))
		}
	}

	if pd := v.PolicyDetail; pd != nil {
		if pd.ShortTitle != "" {
			fmt.Fprintf(&b, "**%s**\n\n", htmlToMarkdown(pd.ShortTitle))
		}
		if pd.RiskDescription != "" {
			fmt.Fprintf(&b, "**Risk** (%s): %s\n\n", pd.Risk, htmlToMarkdown(pd.RiskDescription))
		}
		if pd.EffortDescription != "" {
			fmt.Fprintf(&b, "**Effort** (%s): %s\n\n", pd.Effort, htmlToMarkdown(pd.EffortDescription))
		}
		if pd.DowntimeDescription != "" {
			fmt.Fprintf(&b, "**Downtime** (%s): %s\n\n", pd.Downtime, htmlToMarkdown(pd.DowntimeDescription))
		}
		if pd.AdditionalDetails != "" {
			fmt.Fprintf(&b, "%s\n\n", htmlToMarkdown(pd.AdditionalDetails))
		}
	}

	return b.String()
}

func resolveFilename(projectPath, filename string) string {
	// Resources from remote/registry modules carry a source URL as their
	// filename. Leave it intact - joining it onto projectPath mangles the
	// scheme (https:// -> https:/) and produces a bogus local path.
	if IsRemoteSource(filename) {
		return filename
	}
	if filepath.IsAbs(filename) {
		return filename
	}
	return filepath.Join(projectPath, filename)
}

// IsRemoteSource reports whether a filename is a remote module source URL
// (e.g. a GitHub blob URL or a git:: source) rather than a local file path.
func IsRemoteSource(filename string) bool {
	return strings.Contains(filename, "://") || strings.HasPrefix(filename, "git::")
}

func loadOrGenerateConfig(dir, configTemplate string) (*repoconfig.Config, error) {
	env := envToMap()

	configPath := filepath.Join(dir, "infracost.yml")
	if _, err := os.Stat(configPath); err == nil {
		slog.Debug("loadConfig: found infracost.yml", "path", configPath)
		return repoconfig.LoadConfigFile(configPath, dir, env)
	}

	slog.Debug("loadConfig: no infracost.yml, auto-generating config", "dir", dir)

	opts := []repoconfig.GenerationOption{
		repoconfig.WithEnvVars(env),
	}

	if configTemplate != "" {
		opts = append(opts, repoconfig.WithTemplate(configTemplate))
	} else {
		tmplPath := filepath.Join(dir, "infracost.yml.tmpl")
		if _, err := os.Stat(tmplPath); err == nil {
			slog.Debug("loadConfig: found template", "path", tmplPath)
			content, err := os.ReadFile(tmplPath) // #nosec G304
			if err == nil {
				opts = append(opts, repoconfig.WithTemplate(string(content)))
			}
		}
	}

	return repoconfig.Generate(context.Background(), dir, opts...)
}

func envToMap() map[string]string {
	env := make(map[string]string)
	for _, e := range os.Environ() {
		k, v, ok := strings.Cut(e, "=")
		if ok {
			env[k] = v
		}
	}
	return env
}

type regexReplacement struct {
	re  *regexp.Regexp
	rep string
}

var (
	replaceRegexes = []regexReplacement{
		{regexp.MustCompile(`<a\s+[^>]*href=["']([^"']*)["'][^>]*>(.*?)</a>`), "[$2]($1)"},
		{regexp.MustCompile(`<b>(.*?)</b>`), "**${1}**"},
		{regexp.MustCompile(`<strong>(.*?)</strong>`), "**${1}**"},
		{regexp.MustCompile(`<i>(.*?)</i>`), "_${1}_"},
		{regexp.MustCompile(`<em>(.*?)</em>`), "_${1}_"},
	}
	reHTMLTag = regexp.MustCompile(`<[^>]+>`)
)

// htmlToMarkdown converts HTML anchor tags to markdown links and strips
// remaining HTML tags so the output renders cleanly in editors that only
// support markdown (e.g. Zed, Neovim).
func htmlToMarkdown(s string) string {
	for _, rr := range replaceRegexes {
		s = rr.re.ReplaceAllString(s, rr.rep)
	}
	return reHTMLTag.ReplaceAllString(s, "")
}

var supportedCurrencies = map[string]struct{}{
	"USD": {},
	"AED": {},
	"AFN": {},
	"ALL": {},
	"AMD": {},
	"ANG": {},
	"AOA": {},
	"ARS": {},
	"AUD": {},
	"AWG": {},
	"AZN": {},
	"BAM": {},
	"BBD": {},
	"BDT": {},
	"BGN": {},
	"BHD": {},
	"BIF": {},
	"BMD": {},
	"BND": {},
	"BOB": {},
	"BRL": {},
	"BSD": {},
	"BTN": {},
	"BWP": {},
	"BYN": {},
	"BZD": {},
	"CAD": {},
	"CDF": {},
	"CHF": {},
	"CLF": {},
	"CLP": {},
	"CNY": {},
	"COP": {},
	"CRC": {},
	"CUC": {},
	"CUP": {},
	"CVE": {},
	"CZK": {},
	"DJF": {},
	"DKK": {},
	"DOP": {},
	"DZD": {},
	"EGP": {},
	"ERN": {},
	"ETB": {},
	"EUR": {},
	"FJD": {},
	"FKP": {},
	"GBP": {},
	"GEL": {},
	"GGP": {},
	"GHS": {},
	"GIP": {},
	"GMD": {},
	"GNF": {},
	"GTQ": {},
	"GYD": {},
	"HKD": {},
	"HNL": {},
	"HRK": {},
	"HTG": {},
	"HUF": {},
	"IDR": {},
	"ILS": {},
	"IMP": {},
	"INR": {},
	"IQD": {},
	"IRR": {},
	"ISK": {},
	"JEP": {},
	"JMD": {},
	"JOD": {},
	"JPY": {},
	"KES": {},
	"KGS": {},
	"KHR": {},
	"KMF": {},
	"KPW": {},
	"KRW": {},
	"KWD": {},
	"KYD": {},
	"KZT": {},
	"LAK": {},
	"LBP": {},
	"LKR": {},
	"LRD": {},
	"LSL": {},
	"LYD": {},
	"MAD": {},
	"MDL": {},
	"MKD": {},
	"MMK": {},
	"MNT": {},
	"MOP": {},
	"MUR": {},
	"MVR": {},
	"MWK": {},
	"MXN": {},
	"MYR": {},
	"MZN": {},
	"NAD": {},
	"NGN": {},
	"NIO": {},
	"NOK": {},
	"NPR": {},
	"NZD": {},
	"OMR": {},
	"PAB": {},
	"PEN": {},
	"PGK": {},
	"PHP": {},
	"PKR": {},
	"PLN": {},
	"PYG": {},
	"QAR": {},
	"RON": {},
	"RSD": {},
	"RUB": {},
	"RWF": {},
	"SAR": {},
	"SBD": {},
	"SCR": {},
	"SDG": {},
	"SEK": {},
	"SGD": {},
	"SHP": {},
	"SLL": {},
	"SOS": {},
	"SRD": {},
	"SSP": {},
	"STD": {},
	"SVC": {},
	"SYP": {},
	"SZL": {},
	"THB": {},
	"TJS": {},
	"TMT": {},
	"TND": {},
	"TOP": {},
	"TRY": {},
	"TTD": {},
	"TWD": {},
	"TZS": {},
	"UAH": {},
	"UGX": {},
	"UYU": {},
	"UZS": {},
	"VND": {},
	"VUV": {},
	"WST": {},
	"XAF": {},
	"XAG": {},
	"XAU": {},
	"XCD": {},
	"XDR": {},
	"XPF": {},
	"YER": {},
	"ZAR": {},
	"ZMW": {},
}

var currencySymbols = map[string]string{
	"USD": "$",
	"AUD": "A$",
	"BRL": "R$",
	"CAD": "C$",
	"CHF": "CHF ",
	"CNY": "¥",
	"DKK": "kr ",
	"EUR": "€",
	"GBP": "£",
	"HKD": "HK$",
	"INR": "₹",
	"JPY": "¥",
	"KRW": "₩",
	"NOK": "kr ",
	"NZD": "NZ$",
	"SEK": "kr ",
	"SGD": "S$",
	"ZAR": "R",
}

// NormalizeCurrency normalizes an ISO currency code for scanner settings and UI formatting.
func NormalizeCurrency(currency string) string {
	return strings.ToUpper(strings.TrimSpace(currency))
}

func CurrencyOrDefault(currency string) string {
	currency = NormalizeCurrency(currency)
	if currency == "" {
		return "USD"
	}
	if _, ok := supportedCurrencies[currency]; !ok {
		return "USD"
	}
	return currency
}

func useDiskCaches(currency string) bool {
	return CurrencyOrDefault(currency) == "USD"
}

func (s *Scanner) logInfo(message string, fields map[string]any) {
	attrs := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		attrs = append(attrs, k, v)
	}
	slog.Info(message, attrs...)
	if s.OnLog != nil {
		s.OnLog("info", message, fields)
	}
}

func (s *Scanner) logWarn(message string, fields map[string]any) {
	attrs := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		attrs = append(attrs, k, v)
	}
	slog.Warn(message, attrs...)
	if s.OnLog != nil {
		s.OnLog("warn", message, fields)
	}
}

func (s *Scanner) currencyExchangeRate(ctx context.Context, currency, token string) (*rat.Rat, error) {
	currency = CurrencyOrDefault(currency)
	if currency == "USD" {
		s.logInfo("currencyExchangeRate: using base currency", map[string]any{"currency": currency})
		return rat.New(1), nil
	}

	s.exchangeRateMu.Lock()
	if s.exchangeRates != nil {
		if rate, ok := s.exchangeRates[currency]; ok {
			s.exchangeRateMu.Unlock()
			s.logInfo("currencyExchangeRate: using cached rate", map[string]any{"currency": currency, "rate": rate.String()})
			return rate, nil
		}
	} else {
		s.exchangeRates = make(map[string]*rat.Rat)
	}
	s.exchangeRateMu.Unlock()

	s.logInfo("currencyExchangeRate: fetching rate", map[string]any{"currency": currency})
	rate, err := s.fetchCurrencyExchangeRate(ctx, currency, token)
	if err != nil {
		return nil, err
	}
	if rate == nil || rate.IsZero() {
		return nil, fmt.Errorf("empty exchange rate")
	}

	s.exchangeRateMu.Lock()
	s.exchangeRates[currency] = rate
	s.exchangeRateMu.Unlock()
	s.logInfo("currencyExchangeRate: fetched rate", map[string]any{"currency": currency, "rate": rate.String()})
	return rate, nil
}

func (s *Scanner) fetchCurrencyExchangeRate(ctx context.Context, currency, token string) (*rat.Rat, error) {
	endpoint := s.PricingEndpoint
	if endpoint == "" {
		endpoint = "https://pricing.api.infracost.io"
	}
	url := strings.TrimRight(endpoint, "/") + "/graphql"

	body, err := json.Marshal(map[string]any{
		"query": fmt.Sprintf(`query CurrencyExchangeRate {
  products(filter: {vendorName: "aws", service: "AmazonS3", productFamily: "Storage", region: "us-east-1"}) {
    prices(filter: {unit: "GB-Mo"}) {
      USD
      %s
    }
  }
}`, currency),
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" && s.HTTPClient == nil {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := s.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	s.logInfo("currencyExchangeRate: sending request", map[string]any{"currency": currency, "url": url})
	resp, err := client.Do(req)
	if err != nil {
		s.logWarn("currencyExchangeRate: request failed", map[string]any{"currency": currency, "url": url, "error": err.Error()})
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	s.logInfo("currencyExchangeRate: response received", map[string]any{"currency": currency, "status": resp.Status})
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.logWarn("currencyExchangeRate: error response body", map[string]any{"currency": currency, "status": resp.Status, "body": truncateLogString(string(respBody), 1000)})
		return nil, fmt.Errorf("pricing API status: %s", resp.Status)
	}

	var out struct {
		Data struct {
			Products []struct {
				Prices []map[string]json.RawMessage `json:"prices"`
			} `json:"products"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, err
	}
	if len(out.Errors) > 0 {
		s.logWarn("currencyExchangeRate: graphql error", map[string]any{"currency": currency, "error": out.Errors[0].Message})
		return nil, fmt.Errorf("pricing API error: %s", out.Errors[0].Message)
	}
	for _, product := range out.Data.Products {
		for _, price := range product.Prices {
			usd, err := parseExchangeRate(price["USD"])
			if err != nil || usd == nil || usd.IsZero() {
				continue
			}
			target, err := parseExchangeRate(price[currency])
			if err != nil || target == nil || target.IsZero() {
				continue
			}
			return target.Div(usd), nil
		}
	}
	return nil, fmt.Errorf("pricing API returned no usable %s prices for exchange rate", currency)
}

func truncateLogString(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

func (s *Scanner) fallbackExchangeRate(_ string) *rat.Rat {
	return rat.New(1)
}

func parseExchangeRate(raw json.RawMessage) (*rat.Rat, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("empty exchange rate")
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return rat.NewFromString(s)
	}

	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	return rat.NewFromString(strconv.FormatFloat(f, 'f', -1, 64))
}

// FormatCost formats a rat.Rat as a USD string like "$1.23".
func FormatCost(cost *rat.Rat) string {
	return FormatCostCurrency(cost, "USD")
}

// FormatCostCurrency formats a rat.Rat as a currency string like "$1.23" or "€1.23".
func FormatCostCurrency(cost *rat.Rat, currency string) string {
	amount := 0.0
	if cost != nil && !cost.IsZero() {
		amount = cost.Float64()
	}
	return formatCurrencyAmount(amount, currency, 2)
}

// FormatPriceCurrency formats a unit price using four decimal places.
func FormatPriceCurrency(price *rat.Rat, currency string) string {
	amount := 0.0
	if price != nil && !price.IsZero() {
		amount = price.Float64()
	}
	return formatCurrencyAmount(amount, currency, 4)
}

func formatCurrencyAmount(amount float64, currency string, precision int) string {
	currency = NormalizeCurrency(currency)
	if currency == "" {
		currency = "USD"
	}
	if symbol, ok := currencySymbols[currency]; ok {
		return fmt.Sprintf("%s%.*f", symbol, precision, amount)
	}
	return fmt.Sprintf("%s %.*f", currency, precision, amount)
}
