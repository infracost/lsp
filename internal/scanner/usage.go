package scanner

import (
	"io"
	"slices"

	goprotoevent "github.com/infracost/go-proto/pkg/event"
	"github.com/infracost/go-proto/pkg/rat"

	repoconfig "github.com/infracost/config"
	"github.com/infracost/proto/gen/go/infracost/parser/event"
	"github.com/infracost/proto/gen/go/infracost/usage"
)

// loadUsageDefaults converts API usage defaults into the proto usage format,
// filtered by project name if specified.
func loadUsageDefaults(defaults *event.UsageDefaults, projectName string) *usage.Usage {
	if defaults == nil {
		return nil
	}

	byResourceType := make(map[string]*usage.UsageItemMap, len(defaults.Resources))
	for resourceType, value := range defaults.Resources {
		resourceTypes := make(map[string]*usage.UsageValue, len(value.Usages))
		for attr, attrUsage := range value.Usages {
			list := make([]*event.UsageDefault, len(attrUsage.List))
			copy(list, attrUsage.List)
			slices.SortFunc(list, func(a, b *event.UsageDefault) int {
				if a.Priority > b.Priority {
					return -1
				}
				if a.Priority < b.Priority {
					return 1
				}
				return 0
			})
			for _, item := range list {
				if item.Quantity == "" {
					continue
				}

				if !goprotoevent.StringFilterFromProto(item.GetFilters().GetProject()).Matches(projectName) {
					continue
				}

				if q, err := rat.NewFromString(item.Quantity); err == nil {
					resourceTypes[attr] = &usage.UsageValue{
						Value: &usage.UsageValue_NumberValue{
							NumberValue: q.Proto(),
						},
					}
					break
				}
			}
		}
		byResourceType[resourceType] = &usage.UsageItemMap{
			Items: resourceTypes,
		}
	}
	return &usage.Usage{
		ByResourceType: byResourceType,
	}
}

// loadUsageData loads usage data from a YAML file, merging on top of defaults.
func loadUsageData(r io.Reader, defaults *usage.Usage) (*usage.Usage, error) {
	return repoconfig.LoadUsageYAML(r, defaults)
}
