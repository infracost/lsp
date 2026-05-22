package trace

import (
	"regexp"
	"testing"
)

func TestTraceIDFormat(t *testing.T) {
	re := regexp.MustCompile(`^infracost-lsp-[a-z0-9]{8}-[a-z0-9]{8}$`)
	if !re.MatchString(ID) {
		t.Errorf("trace ID %s does not match expected format", ID)
	}
}
