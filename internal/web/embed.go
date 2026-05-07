package web

import (
	"embed"
	"errors"
	"io/fs"
)

// embeddedDist holds the production React bundle built from
// internal/web/frontend. The build step (npm run build, run from frontend/)
// writes index.html and assets/* into internal/web/dist before the Go binary
// is compiled, and this directive bakes those files into the binary so a
// single artifact ships the full Hearth 2.0 stack.
//
// When the dist directory is present but lacks index.html (e.g. the built
// assets were deleted during development), distFS returns an error and the
// router falls back to a minimal placeholder HTML page so the daemon is still
// useful. The dist/ directory is committed, so a normal checkout includes valid
// built assets and no separate build step is required.
//
//go:embed dist
var embeddedDist embed.FS

// distFS returns the embedded UI filesystem rooted at the dist directory,
// or an error if the embed produced no usable files. The router uses the
// error path to swap in placeholderHTML for the root page.
func distFS() (fs.FS, error) {
	sub, err := fs.Sub(embeddedDist, "dist")
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(sub, ".")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.Name() == "index.html" {
			return sub, nil
		}
	}
	return nil, errors.New("web: no index.html in embedded dist — run `npm run build` in internal/web/frontend")
}
