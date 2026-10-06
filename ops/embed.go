// Package ops contains the system integration shipped inside each signed binary.
package ops

import "embed"

// Files remain embedded in bin/dozor so older updaters can extract the release
// without changing their archive allowlist.
//
//go:embed 50-dozor.rules dozor-recover.service dozor-rollback.service dozor-update.service
var Files embed.FS
