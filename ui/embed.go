//go:build ui

// Package ui embeds the built federated remote (npm run build) when compiled
// with -tags ui; without the tag the service serves no remote.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Remote is the built remote rooted at dist/ (mf-manifest.json at its root).
func Remote() (fs.FS, bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, false
	}
	if _, err := fs.Stat(sub, "mf-manifest.json"); err != nil {
		return nil, false
	}
	return sub, true
}
