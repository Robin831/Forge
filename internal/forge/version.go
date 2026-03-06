// Package forge provides core types and constants for The Forge orchestrator.
package forge

import "runtime/debug"

// Version and Build are set at build time via ldflags:
//
//	go build -ldflags "-X github.com/Robin831/Forge/internal/forge.Version=1.0.0
//	                    -X github.com/Robin831/Forge/internal/forge.Build=abc1234"
//
// When Build is not set via ldflags (remains "unknown"), init() attempts to
// derive it from Go's VCS build info (runtime/debug.ReadBuildInfo). This
// requires the binary to be built from a VCS-enabled module (e.g. a git
// checkout with go build, not go run). A "-dirty" suffix is appended when
// the working tree was modified at build time.
var (
	Version = "dev"
	Build   = "unknown"
)

func init() {
	if Build != "unknown" {
		return
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	var revision, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if revision != "" {
		if len(revision) > 7 {
			revision = revision[:7]
		}
		Build = revision + dirty
	}
}
