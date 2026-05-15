package scanner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/infracost/go-proto/pkg/rat"
	"github.com/infracost/proto/gen/go/infracost/provider"
)

func TestConvertQuantityToMonthly(t *testing.T) {
	tests := []struct {
		name   string
		qty    *rat.Rat
		period provider.Period
		want   *rat.Rat
	}{
		{
			name:   "monthly returns unchanged",
			qty:    rat.New(100),
			period: provider.Period_MONTH,
			want:   rat.New(100),
		},
		{
			name:   "hourly multiplied by 730",
			qty:    rat.New(1),
			period: provider.Period_HOUR,
			want:   rat.New(730),
		},
		{
			name:   "hourly fractional quantity",
			qty:    rat.New(2),
			period: provider.Period_HOUR,
			want:   rat.New(1460),
		},
		{
			name:   "unknown period returns zero",
			qty:    rat.New(100),
			period: provider.Period(999),
			want:   rat.Zero,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertQuantityToMonthly(tt.qty, tt.period)
			assert.True(t, got.Equals(tt.want), "convertQuantityToMonthly(%v, %v) = %v, want %v", tt.qty, tt.period, got, tt.want)
		})
	}
}

func TestApplyDiscount(t *testing.T) {
	half := mustRat(t, "0.5")

	tests := []struct {
		name     string
		price    *rat.Rat
		discount *rat.Rat
		want     *rat.Rat
	}{
		{
			name:     "nil discount returns price unchanged",
			price:    rat.New(100),
			discount: nil,
			want:     rat.New(100),
		},
		{
			name:     "zero discount returns price unchanged",
			price:    rat.New(100),
			discount: rat.Zero,
			want:     rat.New(100),
		},
		{
			name:     "50% discount halves price",
			price:    rat.New(100),
			discount: half,
			want:     rat.New(50),
		},
		{
			name:     "100% discount returns zero",
			price:    rat.New(100),
			discount: rat.New(1),
			want:     rat.Zero,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := applyDiscount(tt.price, tt.discount)
			assert.True(t, got.Equals(tt.want), "applyDiscount(%v, %v) = %v, want %v", tt.price, tt.discount, got, tt.want)
		})
	}
}

func TestFormatCost(t *testing.T) {
	onePointFive := mustRat(t, "1.5")
	oneCent := mustRat(t, "0.01")

	tests := []struct {
		name string
		cost *rat.Rat
		want string
	}{
		{"nil", nil, "$0.00"},
		{"zero", rat.Zero, "$0.00"},
		{"whole number", rat.New(100), "$100.00"},
		{"fractional", onePointFive, "$1.50"},
		{"small amount", oneCent, "$0.01"},
		{"large amount", rat.New(99999), "$99999.00"},
		{"negative", rat.New(-50), "$-50.00"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatCost(tt.cost)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFormatCostCurrency(t *testing.T) {
	tests := []struct {
		name     string
		cost     *rat.Rat
		currency string
		want     string
	}{
		{"eur symbol", rat.New(100), "EUR", "€100.00"},
		{"gbp symbol", mustRat(t, "1.5"), "gbp", "£1.50"},
		{"unknown currency falls back to code", rat.New(25), "ABC", "ABC 25.00"},
		{"empty currency defaults to USD", rat.New(25), "", "$25.00"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatCostCurrency(tt.cost, tt.currency)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestConvertFinopsViolationsConvertsSavingsForHardcodedResource(t *testing.T) {
	finops := []*provider.FinopsPolicyResult{
		{
			PolicyName: "Use cheaper option",
			FailingResources: []*provider.FinopsPolicyFailingResource{
				{
					CauseAddress: "aws_db_instance.my_db",
					Issues: []*provider.FinopsResourceIssue{
						{
							Description:    "Use cheaper option",
							MonthlySavings: rat.New(100).Proto(),
						},
					},
				},
			},
		},
	}
	resources := []*provider.Resource{
		{
			Name: "aws_db_instance.my_db",
			Costs: &provider.ResourceCosts{Components: []*provider.CostComponent{
				{PriceWasHardcoded: true},
			}},
		},
	}

	violations := convertFinopsViolations(finops, resources, "/project", nil, "GBP", mustRat(t, "0.75"))

	require.Len(t, violations, 1)
	assert.True(t, violations[0].MonthlySavings.Equals(rat.New(75)), "monthly savings = %v", violations[0].MonthlySavings)
}

func TestResourceCostConvertsHardcodedPrices(t *testing.T) {
	resource := &provider.Resource{
		Costs: &provider.ResourceCosts{
			Components: []*provider.CostComponent{
				{
					Name:              "Hardcoded compute",
					Unit:              "hours",
					PriceWasHardcoded: true,
					PeriodPrice: &provider.PeriodPrice{
						Price:  rat.New(1).Proto(),
						Period: provider.Period_HOUR,
					},
					Quantity: rat.New(1).Proto(),
				},
				{
					Name: "API price already converted",
					Unit: "months",
					PeriodPrice: &provider.PeriodPrice{
						Price:  rat.New(10).Proto(),
						Period: provider.Period_MONTH,
					},
					Quantity: rat.New(1).Proto(),
				},
			},
		},
	}

	gotCost, gotComponents := resourceCost(resource, mustRat(t, "0.5"))

	assert.True(t, gotCost.Equals(rat.New(375)), "cost = %v", gotCost)
	require.Len(t, gotComponents, 2)
	assert.True(t, gotComponents[0].Price.Equals(mustRat(t, "0.5")), "hardcoded price = %v", gotComponents[0].Price)
	assert.True(t, gotComponents[1].Price.Equals(rat.New(10)), "api price = %v", gotComponents[1].Price)
}

func TestCurrencyExchangeRateReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	s := &Scanner{PricingEndpoint: server.URL, HTTPClient: server.Client()}

	_, err := s.currencyExchangeRate(context.Background(), "GBP", "token")
	require.Error(t, err)
}

func TestCurrencyExchangeRateCachesFetchedRate(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var req struct {
			Query string `json:"query"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Contains(t, req.Query, "GBP")
		_, _ = w.Write([]byte(`{"data":{"products":[{"prices":[{"USD":"2","GBP":"1.5"}]}]}}`))
	}))
	defer server.Close()

	s := &Scanner{PricingEndpoint: server.URL, HTTPClient: server.Client()}

	rate, err := s.currencyExchangeRate(context.Background(), "GBP", "token")
	require.NoError(t, err)
	assert.True(t, rate.Equals(mustRat(t, "0.75")), "rate = %v", rate)
	_, err = s.currencyExchangeRate(context.Background(), "GBP", "token")
	require.NoError(t, err)
	assert.Equal(t, 1, requests)
}

func TestParseExchangeRate(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want *rat.Rat
	}{
		{"string", `"0.7485"`, mustRat(t, "0.7485")},
		{"number", `0.864`, mustRat(t, "0.864")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseExchangeRate(json.RawMessage(tt.raw))
			require.NoError(t, err)
			assert.True(t, got.Equals(tt.want), "rate = %v", got)
		})
	}
}

func TestUseDiskCaches(t *testing.T) {
	assert.True(t, useDiskCaches("USD"))
	assert.True(t, useDiskCaches(""))
	assert.False(t, useDiskCaches("GBP"))
	assert.False(t, useDiskCaches("EUR"))
}

func TestResolveFilename(t *testing.T) {
	tests := []struct {
		name        string
		projectPath string
		filename    string
		want        string
	}{
		{"absolute path returned as-is", "/project", "/abs/path/main.tf", "/abs/path/main.tf"},
		{"relative path joined", "/project", "modules/main.tf", "/project/modules/main.tf"},
		{"simple filename", "/project", "main.tf", "/project/main.tf"},
		{"empty filename", "/project", "", "/project"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveFilename(tt.projectPath, tt.filename)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResourceCost(t *testing.T) {
	tests := []struct {
		name           string
		resource       *provider.Resource
		wantCost       *rat.Rat
		wantComponents int
	}{
		{
			name:           "nil costs",
			resource:       &provider.Resource{},
			wantCost:       rat.Zero,
			wantComponents: 0,
		},
		{
			name: "single hourly component",
			resource: &provider.Resource{
				Costs: &provider.ResourceCosts{
					Components: []*provider.CostComponent{
						{
							Name: "Compute",
							Unit: "hours",
							PeriodPrice: &provider.PeriodPrice{
								Price:  rat.New(1).Proto(),
								Period: provider.Period_HOUR,
							},
							Quantity: rat.New(1).Proto(),
						},
					},
				},
			},
			wantCost:       rat.New(730),
			wantComponents: 1,
		},
		{
			name: "child resources summed",
			resource: &provider.Resource{
				Costs: &provider.ResourceCosts{
					Components: []*provider.CostComponent{
						{
							Name: "Parent",
							Unit: "months",
							PeriodPrice: &provider.PeriodPrice{
								Price:  rat.New(10).Proto(),
								Period: provider.Period_MONTH,
							},
							Quantity: rat.New(1).Proto(),
						},
					},
				},
				ChildResources: []*provider.Resource{
					{
						Costs: &provider.ResourceCosts{
							Components: []*provider.CostComponent{
								{
									Name: "Child",
									Unit: "months",
									PeriodPrice: &provider.PeriodPrice{
										Price:  rat.New(5).Proto(),
										Period: provider.Period_MONTH,
									},
									Quantity: rat.New(1).Proto(),
								},
							},
						},
					},
				},
			},
			wantCost:       rat.New(15),
			wantComponents: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCost, gotComponents := resourceCost(tt.resource, rat.New(1))
			assert.True(t, gotCost.Equals(tt.wantCost), "resourceCost() cost = %v, want %v", gotCost, tt.wantCost)
			assert.Equal(t, tt.wantComponents, len(gotComponents))
		})
	}
}

func mustRat(t *testing.T, s string) *rat.Rat {
	t.Helper()
	r, err := rat.NewFromString(s)
	if err != nil {
		t.Fatalf("NewFromString(%q): %v", s, err)
	}
	return r
}
