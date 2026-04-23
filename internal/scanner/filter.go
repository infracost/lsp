package scanner

import (
	"regexp"
	"strings"

	"github.com/infracost/proto/gen/go/infracost/parser/event"
)

const wildcardPlaceholder = "\x00WILDCARD\x00"

// evaluateProductionFilters checks whether a project should be marked as production
// based on the configured production filters.
func evaluateProductionFilters(filters []*event.ProductionFilter, repositoryName, branchName, projectName string) bool {
	for _, filter := range filters {
		switch filter.Type {
		case event.ProductionFilter_BRANCH:
			if matchProductionFilter(filter.Value, branchName) {
				return filter.Include
			}
		case event.ProductionFilter_PROJECT:
			if matchProductionFilter(filter.Value, projectName) {
				return filter.Include
			}
		case event.ProductionFilter_REPO:
			if matchProductionFilter(filter.Value, repositoryName) {
				return filter.Include
			}
		}
	}
	return false
}

// matchProductionFilter evaluates production filter patterns with wildcard support.
func matchProductionFilter(matcher, value string) bool {
	escaped := strings.ReplaceAll(matcher, "*", wildcardPlaceholder)
	escaped = regexp.QuoteMeta(escaped)

	if !strings.HasSuffix(escaped, wildcardPlaceholder) {
		escaped += "\\b"
	}
	if !strings.HasPrefix(escaped, wildcardPlaceholder) {
		escaped = "\\b" + escaped
	}
	escaped = strings.ReplaceAll(escaped, wildcardPlaceholder, ".*")

	re, err := regexp.Compile(escaped)
	if err != nil {
		return false
	}
	return re.MatchString(value)
}
