// Package forge provides core types and constants for The Forge orchestrator.
package forge

import "runtime/debug"

// Version and Build are set at build time via ldflags:
//
//	go build -ldflags "-X github.com/Robin831/Forge/internal/forge.Version=1.0.0
//	                    -X github.com/Robin831/Forge/internal/forge.Build=abc1234"
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
