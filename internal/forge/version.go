// Package forge provides core types and constants for The Forge orchestrator.
package forge

// Version and Build are set at build time via ldflags:
//
//	go build -ldflags "-X github.com/Robin831/Forge/internal/forge.Version=1.0.0
//	                    -X github.com/Robin831/Forge/internal/forge.Build=abc1234"
var (
	Version = "dev"
	Build   = "unknown"
)
