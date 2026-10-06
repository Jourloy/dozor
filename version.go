package dozor

import (
	_ "embed"
	"strings"
)

//go:embed VERSION
var projectVersion string

// Version returns the project version embedded when the binary was built.
func Version() string { return strings.TrimSpace(projectVersion) }
