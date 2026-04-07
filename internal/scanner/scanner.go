package scanner

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
	repoconfig "github.com/infracost/config"
	"github.com/infracost/go-proto/pkg/diagnostic"
	goprotoevent "github.com/infracost/go-proto/pkg/event"
	providerconv "github.com/infracost/go-proto/pkg/providers"
	"github.com/infracost/go-proto/pkg/rat"
	parserapi "github.com/infracost/proto/gen/go/infracost/parser/api"
	"github.com/infracost/proto/gen/go/infracost/parser/cloudformation"
	"github.com/infracost/proto/gen/go/infracost/parser/event"
	"github.com/infracost/proto/gen/go/infracost/parser/options"
	"github.com/infracost/proto/gen/go/infracost/parser/terraform"
	"github.com/infracost/proto/gen/go/infracost/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/infracost/lsp/internal/api"
	"github.com/infracost/lsp/internal/dashboard"
	"github.com/infracost/lsp/internal/plugins/parser"
	"github.com/infracost/lsp/internal/plugins/providers"
	"github.com/infracost/lsp/internal/vcs"
)

// Scanner orchestrates parsing and pricing of IaC projects.
type Scanner struct {
	Parser            *parser.PluginClient
	Provider          *providers.PluginClient
	EnsureProvider    func(provider.Provider) error
	LogLevel          hclog.Level
	Currency          string
	PricingEndpoint   string
	DashboardEndpoint string
	TokenSource       *api.TokenSource
	HTTPClient        *http.Client
	OnOrgID           func(string)

	tagPolicies        []*event.TagPolicy
	finopsPolicies     []*event.FinopsPolicySettings
	runParamsOrgID     string
	runParamsFetchedAt time.Time
	runParamsTTL       time.Duration

	policyDetailCache map[string]dashboard.PolicyDetail
}

// Init initializes internal state. Must be called before first use.
func (s *Scanner) Init() {
	s.policyDetailCache = make(map[string]dashboard.PolicyDetail)
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

// SetRunParamsTTL sets how long fetchRunParams results are cached.
func (s *Scanner) SetRunParamsTTL(d time.Duration) {
	s.runParamsTTL = d
	slog.Info("scanner: runParams cache TTL set", "ttl", d)
}

// fetchRunParams queries the dashboard API for run parameters (org ID, tag policies, etc.)
// and stores parsed tag policies on the scanner. Returns org ID (empty on failure, non-fatal).
func (s *Scanner) fetchRunParams(ctx context.Context, rootDir string) string {
	if !s.TokenSource.Valid() {
		return ""
	}

	if s.runParamsTTL > 0 && !s.runParamsFetchedAt.IsZero() && time.Since(s.runParamsFetchedAt) < s.runParamsTTL {
		slog.Debug("fetchRunParams: using cached result", "org_id", s.runParamsOrgID, "age", time.Since(s.runParamsFetchedAt))
		return s.runParamsOrgID
	}

	repoURL := vcs.GetRemoteURL(rootDir)
	branch := vcs.GetCurrentBranch(rootDir)

	client := dashboard.NewClient(s.HTTPClient, s.DashboardEndpoint)
	params, err := client.RunParameters(ctx, "", repoURL, branch)
	if err != nil {
		slog.Warn("fetchRunParams: failed to get run parameters", "error", err)
		return ""
	}

	s.tagPolicies = nil
	for i, raw := range params.TagPolicies {
		slog.Debug("fetchRunParams: raw tag policy", "index", i, "json", string(raw))
		var tp event.TagPolicy
		if err := protojson.Unmarshal(raw, &tp); err != nil {
			slog.Warn("fetchRunParams: failed to unmarshal tag policy", "error", err)
			continue
		}
		s.tagPolicies = append(s.tagPolicies, &tp)
	}

	s.finopsPolicies = nil
	for i, raw := range params.FinopsPolicies {
		slog.Debug("fetchRunParams: raw finops policy", "index", i, "json", string(raw))
		var fp event.FinopsPolicySettings
		if err := protojson.Unmarshal(raw, &fp); err != nil {
			slog.Warn("fetchRunParams: failed to unmarshal finops policy", "error", err)
			continue
		}
		s.finopsPolicies = append(s.finopsPolicies, &fp)
	}

	s.runParamsOrgID = params.OrganizationID
	s.runParamsFetchedAt = time.Now()

	if s.OnOrgID != nil {
		s.OnOrgID(params.OrganizationID)
	}

	slog.Debug("fetchRunParams: resolved", "org_id", params.OrganizationID, "tag_policies", len(s.tagPolicies))
	return params.OrganizationID
}

// Close kills all persistent plugin subprocesses.
func (s *Scanner) Close() {
	s.Parser.Close()
	s.Provider.Close()
}

// LoadConfig loads or auto-generates an infracost config for the given directory.
func LoadConfig(dir string) (*repoconfig.Config, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolving path: %w", err)
	}
	return loadOrGenerateConfig(absDir)
}

// Scan analyzes the given directory and returns resource cost results.
func (s *Scanner) Scan(ctx context.Context, dir string) (*ScanResult, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolving path: %w", err)
	}

	cfg, err := loadOrGenerateConfig(absDir)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	return s.ScanAll(ctx, absDir, cfg)
}

// ScanAll scans all projects in the given config.
func (s *Scanner) ScanAll(ctx context.Context, rootDir string, cfg *repoconfig.Config) (*ScanResult, error) {
	result := &ScanResult{}

	orgID := s.fetchRunParams(ctx, rootDir)
	slog.Debug("scanAll: starting", "projects", len(cfg.Projects), "currency", cfg.Currency, "org_id", orgID)

	for _, project := range cfg.Projects {
		r, err := s.scanProject(ctx, rootDir, cfg, project, orgID)
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
	orgID := s.fetchRunParams(ctx, rootDir)
	return s.scanProject(ctx, rootDir, cfg, project, orgID)
}

func (s *Scanner) scanProject(ctx context.Context, rootDir string, cfg *repoconfig.Config, project *repoconfig.Project, orgID string) (*ScanResult, error) {
	projectPath := filepath.Clean(filepath.Join(rootDir, project.Path))
	slog.Debug("scanProject: starting", "name", project.Name, "path", projectPath)

	parseStart := time.Now()
	parseResp, err := s.parse(ctx, projectPath, cfg, project, rootDir)
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

	if parseResp.Result == nil {
		return nil, fmt.Errorf("parser returned no result")
	}

	var requiredProviders []provider.Provider
	srcLocs := make(map[string]sourceLocation)
	modLocs := make(map[string]sourceLocation)
	switch pr := parseResp.Result.Value.(type) {
	case *parserapi.ParseResponseResult_Terraform:
		rps := make(map[provider.Provider]struct{})
		getRequiredProviders(pr.Terraform, rps)
		requiredProviders = slices.Collect(maps.Keys(rps))
		getSourceLocations(pr.Terraform, "", srcLocs, modLocs)
		slog.Debug("scanProject: source locations", "count", len(srcLocs), "module_locations", len(modLocs), "providers", requiredProviders)
	case *parserapi.ParseResponseResult_Cloudformation:
		requiredProviders = []provider.Provider{provider.Provider_PROVIDER_AWS}
	default:
		return nil, fmt.Errorf("unsupported parse result type: %T", pr)
	}

	currency := s.Currency
	if currency == "" {
		currency = "USD"
	}

	pricingEndpoint := s.PricingEndpoint
	if pricingEndpoint == "" {
		pricingEndpoint = "https://pricing.api.infracost.io"
	}

	token, err := s.accessToken()
	if err != nil {
		return nil, fmt.Errorf("authentication: %w", err)
	}

	input := &provider.Input{
		ParseResult:  parseResp,
		AbsolutePath: projectPath,
		ProjectInfo: &provider.ProjectInfo{
			Name: project.Name,
		},
		Features: &provider.Features{
			EnablePriceLookups:   true,
			EnableFinopsPolicies: true,
		},
		FinopsPolicyConfig: &provider.FinopsPolicyConfiguration{
			Policies: s.finopsPolicies,
		},
		Settings: &provider.Settings{
			Currency:      currency,
			UseDiskCaches: true,
		},
		Infracost: &provider.Infracost{
			ApiKey:             token,
			PricingApiEndpoint: pricingEndpoint,
			OrgId:              &orgID,
		},
	}

	var allResources []*provider.Resource
	var allFinops []*provider.FinopsPolicyResult

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

	result.Resources = make([]ResourceResult, 0, len(allResources))
	for _, r := range allResources {
		monthlyCost, components := resourceCost(r)

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

	result.Violations = convertFinopsViolations(allFinops, allResources, rootDir, srcLocs)

	if len(result.Violations) > 0 && s.HasTokenSource() && orgID != "" {
		s.attachPolicyDetails(ctx, orgID, result.Violations)
	}

	if len(s.tagPolicies) > 0 {
		tagResults := goprotoevent.TagPolicies(s.tagPolicies).EvaluateAgainstResources(allResources, input.ProjectInfo)
		result.TagViolations = convertTagViolations(tagResults, allResources, rootDir, srcLocs)
		slog.Debug("scanProject: tag policy evaluation", "results", len(tagResults), "violations", len(result.TagViolations))
	}

	return result, nil
}

func (s *Scanner) attachPolicyDetails(ctx context.Context, orgID string, violations []FinopsViolation) {
	var uncached []string
	for _, v := range violations {
		if v.PolicySlug == "" {
			continue
		}
		if _, ok := s.policyDetailCache[v.PolicySlug]; !ok {
			uncached = append(uncached, v.PolicySlug)
		}
	}

	if len(uncached) > 0 {
		client := dashboard.NewClient(s.HTTPClient, s.DashboardEndpoint)
		details, err := client.PolicyDetails(ctx, orgID, uncached)
		if err != nil {
			slog.Warn("attachPolicyDetails: failed to fetch policy details", "error", err)
			return
		}
		for slug, pd := range details {
			s.policyDetailCache[slug] = pd
		}
	}

	for i := range violations {
		if pd, ok := s.policyDetailCache[violations[i].PolicySlug]; ok {
			pd := pd // copy for pointer
			violations[i].PolicyDetail = &pd
			violations[i].Markdown = buildViolationMarkdown(violations[i])
		}
	}

	slog.Debug("attachPolicyDetails: attached", "uncached", len(uncached), "total_cached", len(s.policyDetailCache))
}

func (s *Scanner) processProvider(ctx context.Context, prov provider.Provider, input *provider.Input) ([]*provider.Resource, []*provider.FinopsPolicyResult, []string) {
	name := providerconv.FromProto(prov)
	if s.EnsureProvider != nil {
		if err := s.EnsureProvider(prov); err != nil {
			slog.Error("processProvider: failed to ensure plugin", "provider", name, "error", err)
			return nil, nil, []string{fmt.Sprintf("ensuring %s provider: %v", name, err)}
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	loadStart := time.Now()
	client, err := s.Provider.Load(prov, s.LogLevel)
	loadDuration := time.Since(loadStart)
	if err != nil {
		slog.Error("processProvider: failed to load plugin", "provider", name, "error", err, "elapsed", loadDuration)
		return nil, nil, []string{fmt.Sprintf("loading %s provider: %v", name, err)}
	}
	slog.Debug("processProvider: plugin loaded", "provider", name, "elapsed", loadDuration)

	processStart := time.Now()
	resp, err := client.Process(ctx, &provider.ProcessRequest{Input: input})
	processDuration := time.Since(processStart)
	if err != nil {
		if isTransportError(err) {
			slog.Warn("processProvider: transport error, reconnecting", "provider", name, "error", err)
			client, err = s.Provider.Reconnect(prov, s.LogLevel)
			if err != nil {
				return nil, nil, []string{fmt.Sprintf("reconnecting %s provider: %v", name, err)}
			}
			resp, err = client.Process(ctx, &provider.ProcessRequest{Input: input})
			processDuration = time.Since(processStart)
			if err != nil {
				slog.Error("processProvider: Process failed after reconnect", "provider", name, "error", err, "elapsed", processDuration)
				return nil, nil, []string{fmt.Sprintf("%s provider error: %v", name, err)}
			}
		} else {
			slog.Error("processProvider: Process failed", "provider", name, "error", err, "elapsed", processDuration)
			return nil, nil, []string{fmt.Sprintf("%s provider error: %v", name, err)}
		}
	}

	slog.Debug("processProvider: complete", "provider", name, "resources", len(resp.Output.Resources), "finops", len(resp.Output.FinopsResults), "elapsed", processDuration)
	return resp.Output.Resources, resp.Output.FinopsResults, nil
}

func (s *Scanner) parse(ctx context.Context, path string, cfg *repoconfig.Config, project *repoconfig.Project, rootDir string) (*parserapi.ParseResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	slog.Debug("parse: loading parser plugin", "plugin_path", s.Parser.Plugin)
	client, err := s.Parser.Load(s.LogLevel)
	if err != nil {
		return nil, fmt.Errorf("loading parser plugin: %w", err)
	}

	cacheDir := filepath.Join(os.TempDir(), ".infracost", "cache")
	_ = os.MkdirAll(cacheDir, 0700)

	genericOpts := &options.GenericOptions{
		ProjectName:        project.Name,
		RepoDirectory:      rootDir,
		TemporaryDirectory: os.TempDir(),
		CacheDirectory:     cacheDir,
		WorkingDirectory:   rootDir,
	}

	var target *parserapi.ParseRequestTarget
	switch project.Type {
	case repoconfig.ProjectTypeCloudFormation:
		target = buildCloudFormationTarget(path, project, genericOpts)
	default:
		target = buildTerraformTarget(path, cfg, project, genericOpts)
	}

	slog.Debug("parse: calling Parse",
		"path", path,
		"project_name", project.Name,
		"project_type", project.Type,
	)

	resp, err := client.Parse(ctx, &parserapi.ParseRequest{
		RepoDirectory:    rootDir,
		WorkingDirectory: rootDir,
		Target:           target,
	})
	if err != nil && isTransportError(err) {
		slog.Warn("parse: transport error, reconnecting", "error", err)
		client, err = s.Parser.Reconnect(s.LogLevel)
		if err != nil {
			return nil, fmt.Errorf("reconnecting parser plugin: %w", err)
		}
		resp, err = client.Parse(ctx, &parserapi.ParseRequest{
			RepoDirectory:    rootDir,
			WorkingDirectory: rootDir,
			Target:           target,
		})
	}
	if err != nil {
		slog.Error("parse: Parse failed", "error", err)
		return resp, err
	}

	slog.Debug("parse: complete", "has_result", resp.Result != nil)
	return resp, nil
}

func buildTerraformTarget(path string, cfg *repoconfig.Config, project *repoconfig.Project, generic *options.GenericOptions) *parserapi.ParseRequestTarget {
	var regexSourceMap map[string]string
	if len(cfg.Terraform.SourceMap) > 0 {
		regexSourceMap = make(map[string]string, len(cfg.Terraform.SourceMap))
		for _, source := range cfg.Terraform.SourceMap {
			regexSourceMap[source.Match] = source.Replace
		}
	}

	return &parserapi.ParseRequestTarget{
		Value: &parserapi.ParseRequestTarget_Terraform{
			Terraform: &terraform.Target{
				Directory: path,
				Options: &terraform.Options{
					Generic:        generic,
					RegexSourceMap: regexSourceMap,
					Env:            project.Env,
					Workspace:      project.Terraform.Workspace,
					TfVarsFiles:    project.Terraform.VarFiles,
				},
			},
		},
	}
}

func buildCloudFormationTarget(path string, project *repoconfig.Project, generic *options.GenericOptions) *parserapi.ParseRequestTarget {
	var awsCtx *cloudformation.AwsContext
	if project.AWS.AccountID != "" || project.AWS.Region != "" || project.AWS.StackID != "" || project.AWS.StackName != "" {
		awsCtx = &cloudformation.AwsContext{
			AccountId: project.AWS.AccountID,
			Region:    project.AWS.Region,
			StackId:   project.AWS.StackID,
			StackName: project.AWS.StackName,
		}
	}

	return &parserapi.ParseRequestTarget{
		Value: &parserapi.ParseRequestTarget_Cloudformation{
			Cloudformation: &cloudformation.Target{
				TemplatePath: path,
				Options: &cloudformation.Options{
					Generic:    generic,
					AwsContext: awsCtx,
				},
			},
		},
	}
}

func getRequiredProviders(result *terraform.ModuleResult, provs map[provider.Provider]struct{}) {
	for _, resource := range result.Resources {
		p, _, _ := strings.Cut(resource.Type, "_")
		pp := providerconv.ToProto(p)
		if pp != provider.Provider_PROVIDER_UNSPECIFIED {
			provs[pp] = struct{}{}
		}
	}
	for _, module := range result.Modules {
		for _, r := range module.Results {
			getRequiredProviders(r, provs)
		}
	}
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

// getSourceLocations walks the terraform parse result and builds a map from
// resource address (e.g. "aws_instance.my_web_app") to source location.
// It also extracts module call locations from resource CallStacks into modLocs.
func getSourceLocations(result *terraform.ModuleResult, prefix string, out map[string]sourceLocation, modLocs map[string]sourceLocation) {
	for _, resource := range result.Resources {
		addr := resource.Type + "." + resource.Name
		if prefix != "" {
			addr = prefix + "." + addr
		}
		if sr := resource.SourceRange; sr != nil {
			out[addr] = sourceLocation{
				Filename:  sr.Filename,
				StartLine: sr.StartLine,
				EndLine:   sr.EndLine,
			}
		}

		if cs := resource.CallStack; cs != nil {
			var modAddr string
			for _, frame := range cs.Frames {
				if frame.Address == nil || frame.SourceRange == nil {
					continue
				}
				parts := make([]string, 0, len(frame.Address.Segments))
				for _, seg := range frame.Address.Segments {
					parts = append(parts, seg.Value)
				}
				segAddr := strings.Join(parts, ".")
				if modAddr != "" {
					modAddr += "." + segAddr
				} else {
					modAddr = segAddr
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
	}
	for name, module := range result.Modules {
		modPrefix := "module." + name
		if prefix != "" {
			modPrefix = prefix + "." + modPrefix
		}
		for _, r := range module.Results {
			getSourceLocations(r, modPrefix, out, modLocs)
		}
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

func convertFinopsViolations(finops []*provider.FinopsPolicyResult, resources []*provider.Resource, projectPath string, srcLocs map[string]sourceLocation) []FinopsViolation {
	resourceMeta := make(map[string]*provider.ResourceMetadata, len(resources))
	for _, r := range resources {
		if r.Metadata != nil {
			resourceMeta[r.Name] = r.Metadata
		}
	}

	var violations []FinopsViolation
	for _, fp := range finops {
		for _, fr := range fp.FailingResources {
			for _, issue := range fr.Issues {
				v := FinopsViolation{
					PolicyID:         fp.PolicyId,
					PolicyName:       fp.PolicyName,
					PolicySlug:       fp.PolicySlug,
					BlockPullRequest: fp.BlockPullRequest,
					Message:          issue.Description,
					Address:          fr.CauseAddress,
					Attribute:        issue.Attribute,
					MonthlySavings:   rat.FromProto(issue.MonthlySavings),
				}
				if issue.SavingsDetails != nil {
					v.SavingsDetails = *issue.SavingsDetails
				}
				v.Markdown = buildViolationMarkdown(v)

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

func buildViolationMarkdown(v FinopsViolation) string {
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
		fmt.Fprintf(&b, "**Potential Savings:** %s/mo\n\n", FormatCost(v.MonthlySavings))
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
	if filepath.IsAbs(filename) {
		return filename
	}
	return filepath.Join(projectPath, filename)
}

func loadOrGenerateConfig(dir string) (*repoconfig.Config, error) {
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

	tmplPath := filepath.Join(dir, "infracost.yml.tmpl")
	if _, err := os.Stat(tmplPath); err == nil {
		slog.Debug("loadConfig: found template", "path", tmplPath)
		content, err := os.ReadFile(tmplPath) // #nosec G304
		if err == nil {
			opts = append(opts, repoconfig.WithTemplate(string(content)))
		}
	}

	return repoconfig.Generate(dir, opts...)
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

// FormatCost formats a rat.Rat as a dollar string like "$1.23".
func FormatCost(cost *rat.Rat) string {
	if cost == nil || cost.IsZero() {
		return "$0.00"
	}
	return fmt.Sprintf("$%.2f", cost.Float64())
}
