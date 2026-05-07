package web

import (
	"embed"
	"errors"
	"io/fs"
)

// distFS is replaced in a future bead with a real //go:embed dist directive
// pointing at the built React bundle. For this skeleton bead we only embed
// a placeholder index.html so go:embed has at least one file to anchor on
// (the directive errors out on an empty pattern).
//
//go:embed dist
var embeddedDist embed.FS

// distFS returns the embedded UI filesystem rooted at the dist directory,
// or an error if the embed produced no files (in which case the router
// falls back to serving placeholderHTML).
func distFS() (fs.FS, error) {
	sub, err := fs.Sub(embeddedDist, "dist")
	if err != nil {
		return nil, err
	}
	// If the dist directory only contains the .gitkeep file, treat it as
	// empty so the placeholder takes over.
	entries, err := fs.ReadDir(sub, ".")
	if err != nil {
		return nil, err
	}
	hasIndex := false
	for _, entry := range entries {
		if entry.Name() == "index.html" {
			hasIndex = true
			break
		}
	}
	if !hasIndex {
		return nil, errors.New("web: no index.html in embedded dist")
	}
	return sub, nil
}
