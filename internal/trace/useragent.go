package trace

import (
	"fmt"

	"github.com/infracost/lsp/version"
)

var UserAgent = fmt.Sprintf("infracost-lsp-%s", version.Version)
