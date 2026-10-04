//go:build !ui

// Package ui embeds the built federated remote when compiled with -tags ui.
package ui

import "io/fs"

// Remote reports no remote in this build.
func Remote() (fs.FS, bool) { return nil, false }
