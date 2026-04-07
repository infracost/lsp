package scanner

import (
	"github.com/infracost/go-proto/pkg/rat"
	"github.com/infracost/proto/gen/go/infracost/provider"

	"github.com/infracost/lsp/internal/dashboard"
)

// ResourceResult holds cost information for a single resource with its source location.
type ResourceResult struct {
	Name      string // e.g. "aws_instance.web"
	Type      string // e.g. "aws_instance"
	Filename  string
	StartLine int64
	EndLine   int64

	MonthlyCost    *rat.Rat
	CostComponents []CostComponent

	IsSupported bool
	IsFree      bool
}

type CostComponent struct {
	Name             string
	Unit             string
	Price            *rat.Rat
	MonthlyQuantity  *rat.Rat
	TotalMonthlyCost *rat.Rat
}

// FinopsViolation represents a FinOps policy violation for a resource.
type FinopsViolation struct {
	PolicyID         string
	PolicyName       string
	PolicySlug       string
	BlockPullRequest bool
	Message          string
	Address          string
	Attribute        string
	Filename         string
	StartLine        int64
	EndLine          int64
	MonthlySavings   *rat.Rat
	SavingsDetails   string
	Markdown         string
	PolicyDetail     *dashboard.PolicyDetail
}

// ModuleCost holds aggregated cost information for a module call block.
type ModuleCost struct {
	Name          string // e.g. "module.dashboard"
	Filename      string
	StartLine     int64
	EndLine       int64
	MonthlyCost   *rat.Rat
	ResourceCount int
}

// TagViolation represents a tagging policy violation for a resource.
type TagViolation struct {
	PolicyID      string
	PolicyName    string
	PolicyMessage string
	BlockPR       bool
	Address       string
	ResourceType  string
	Filename      string
	StartLine     int64
	EndLine       int64
	Message       string
	MissingTags   []string
	InvalidTags   []InvalidTagResult
	Markdown      string
}

// InvalidTagResult describes a tag with an invalid value.
type InvalidTagResult struct {
	Key         string
	Value       string
	Suggestion  string
	Message     string
	ValidValues []string
}

// ScanResult holds all results from scanning a directory.
type ScanResult struct {
	Resources     []ResourceResult
	ModuleCosts   []ModuleCost
	Violations    []FinopsViolation
	TagViolations []TagViolation
	Errors        []string
}

var hoursInMonth = rat.New(730)

func convertQuantityToMonthly(qty *rat.Rat, period provider.Period) *rat.Rat {
	switch period {
	case provider.Period_MONTH:
		return qty
	case provider.Period_HOUR:
		return qty.Mul(hoursInMonth)
	default:
		return rat.Zero
	}
}

func applyDiscount(price *rat.Rat, discountRate *rat.Rat) *rat.Rat {
	if discountRate != nil && discountRate.GreaterThan(rat.Zero) {
		return price.Mul(rat.New(1).Sub(discountRate))
	}
	return price
}

// resourceCost computes the total monthly cost for a provider.Resource.
func resourceCost(r *provider.Resource) (*rat.Rat, []CostComponent) {
	total := rat.Zero
	var components []CostComponent

	if r.Costs != nil {
		for _, c := range r.Costs.Components {
			cc := convertCostComponent(c)
			components = append(components, cc)
			if cc.TotalMonthlyCost != nil {
				total = total.Add(cc.TotalMonthlyCost)
			}
		}
	}

	for _, child := range r.ChildResources {
		childTotal, childComponents := resourceCost(child)
		total = total.Add(childTotal)
		components = append(components, childComponents...)
	}

	return total, components
}

func convertCostComponent(c *provider.CostComponent) CostComponent {
	monthlyQty := rat.Zero
	price := rat.Zero
	totalMonthlyCost := rat.Zero

	if c.PeriodPrice != nil {
		price = applyDiscount(rat.FromProto(c.PeriodPrice.Price), rat.FromProto(c.DiscountRate))
		if c.Quantity != nil {
			monthlyQty = convertQuantityToMonthly(rat.FromProto(c.Quantity), c.PeriodPrice.Period)
			totalMonthlyCost = price.Mul(monthlyQty)
		}
	}

	return CostComponent{
		Name:             c.Name,
		Unit:             c.Unit,
		Price:            price,
		MonthlyQuantity:  monthlyQty,
		TotalMonthlyCost: totalMonthlyCost,
	}
}
