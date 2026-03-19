package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/infracost/lsp/internal/trace"
)

func NewClient(httpClient *http.Client, endpoint string) *Client {
	return &Client{
		client:   httpClient,
		endpoint: endpoint,
	}
}

type Client struct {
	client   *http.Client
	endpoint string
}

type RunParameters struct {
	OrganizationID string            `json:"organizationId"`
	RepositoryName string            `json:"repositoryName"`
	TagPolicies    []json.RawMessage `json:"tagPolicies"`
	FinopsPolicies []json.RawMessage `json:"finopsPolicies"`
}

func (c *Client) RunParameters(ctx context.Context, organizationID, repoURL, branchName string) (RunParameters, error) {
	const query = `query RunParameters($repoUrl: String!, $branchName: String!) {
  runParameters(repoUrl: $repoUrl, branchName: $branchName) {
    organizationId
    repositoryName
    tagPolicies
    finopsPolicies
  }
}`

	type response struct {
		RunParameters RunParameters `json:"runParameters"`
	}

	r, err := graphqlQuery[response](ctx, c.organizationScopedClient(organizationID), fmt.Sprintf("%s/graphql", c.endpoint), query, map[string]any{
		"repoUrl":    repoURL,
		"branchName": branchName,
	})
	if err != nil {
		return RunParameters{}, err
	}

	if len(r.Errors) > 0 {
		var errs []string
		for _, e := range r.Errors {
			errs = append(errs, e.Message)
		}
		return r.Data.RunParameters, errors.New(strings.Join(errs, "; "))
	}
	return r.Data.RunParameters, nil
}

// PolicyDetail holds metadata about a FinOps policy from the dashboard.
type PolicyDetail struct {
	Risk                string `json:"risk"`
	Effort              string `json:"effort"`
	Downtime            string `json:"downtime"`
	RiskDescription     string `json:"riskDescription"`
	EffortDescription   string `json:"effortDescription"`
	DowntimeDescription string `json:"downtimeDescription"`
	AdditionalDetails   string `json:"additionalDetails"`
	ShortTitle          string `json:"shortTitle"`
}

// PolicyDetails fetches policy metadata for the given slugs from the dashboard API.
func (c *Client) PolicyDetails(ctx context.Context, orgID string, slugs []string) (map[string]PolicyDetail, error) {
	const query = `query PolicyDetails($slugs: [String!]!) {
  policyDetails(slugs: $slugs) {
    slug
    risk
    effort
    downtime
    riskDescription
    effortDescription
    downtimeDescription
    additionalDetails
    shortTitle
  }
}`

	type policyDetailRow struct {
		Slug string `json:"slug"`
		PolicyDetail
	}

	type response struct {
		PolicyDetails []policyDetailRow `json:"policyDetails"`
	}

	r, err := graphqlQuery[response](ctx, c.organizationScopedClient(orgID), fmt.Sprintf("%s/graphql", c.endpoint), query, map[string]any{
		"slugs": slugs,
	})
	if err != nil {
		return nil, err
	}

	if len(r.Errors) > 0 {
		var errs []string
		for _, e := range r.Errors {
			errs = append(errs, e.Message)
		}
		return nil, errors.New(strings.Join(errs, "; "))
	}

	result := make(map[string]PolicyDetail, len(r.Data.PolicyDetails))
	for _, pd := range r.Data.PolicyDetails {
		result[pd.Slug] = pd.PolicyDetail
	}
	return result, nil
}

func (c *Client) organizationScopedClient(organizationID string) *http.Client {
	return &http.Client{
		Transport: &headerTransport{
			base: c.client.Transport,
			headers: map[string]string{
				"x-infracost-org-id": organizationID,
				"User-Agent":         trace.UserAgent,
			},
		},
		CheckRedirect: c.client.CheckRedirect,
		Jar:           c.client.Jar,
		Timeout:       c.client.Timeout,
	}
}

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h *headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	for k, v := range h.headers {
		if len(v) == 0 {
			continue
		}
		request.Header.Set(k, v)
	}
	return h.base.RoundTrip(request)
}
