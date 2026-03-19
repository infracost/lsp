package trace

import (
	"fmt"

	"github.com/infracost/lsp/version"
)

var (
	UserAgent = fmt.Sprintf("infracost-cliv2-%s", version.Version)
)
