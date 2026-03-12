package scanner

import (
	"testing"

	"github.com/stretchr/testify/assert"

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
			gotCost, gotComponents := resourceCost(tt.resource)
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
