package kiln

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const twoServiceManifest = `version: 1

setup: "./scripts/preview-db-setup.sh"
teardown: "./scripts/preview-db-teardown.sh"

services:
  api:
    command: "dotnet run --project src/Api --no-launch-profile"
    dir: "."
    env:
      ASPNETCORE_URLS: "http://127.0.0.1:{{.Port}}"
      ConnectionStrings__Default: "Server=localhost;Database=app_preview_{{.PreviewID}}"
    health: "/healthz"
    ready_timeout: 120s

  client:
    command: "npm run dev -- --port {{.Port}} --strictPort"
    dir: "client"
    env:
      VITE_API_URL: "http://{{.Host}}:{{.ServicePort \"api\"}}"
    entry: true
    ready_timeout: 60s
`

func TestParseValidManifest(t *testing.T) {
	m, err := Parse([]byte(twoServiceManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if m.Version != 1 {
		t.Errorf("version: got %d, want 1", m.Version)
	}
	if m.Setup != "./scripts/preview-db-setup.sh" {
		t.Errorf("setup: got %q", m.Setup)
	}
	if m.Teardown != "./scripts/preview-db-teardown.sh" {
		t.Errorf("teardown: got %q", m.Teardown)
	}

	// Manifest order is preserved so services start in a predictable order.
	if got := m.Services.Names(); len(got) != 2 || got[0] != "api" || got[1] != "client" {
		t.Fatalf("service order: got %v, want [api client]", got)
	}

	api, ok := m.Services.Get("api")
	if !ok {
		t.Fatal("service api not found")
	}
	if api.Dir != "." {
		t.Errorf("api.dir: got %q, want %q", api.Dir, ".")
	}
	if api.Health != "/healthz" {
		t.Errorf("api.health: got %q", api.Health)
	}
	if api.ReadyTimeout != 120*time.Second {
		t.Errorf("api.ready_timeout: got %s, want 2m0s", api.ReadyTimeout)
	}
	if api.Entry {
		t.Error("api should not be the entry service")
	}
	if len(api.Env) != 2 {
		t.Errorf("api.env: got %d entries, want 2", len(api.Env))
	}
	// Templates are preserved verbatim by the loader; Expand resolves them.
	if !strings.Contains(api.Env["ASPNETCORE_URLS"], "{{.Port}}") {
		t.Errorf("api.env ASPNETCORE_URLS should keep its template: %q", api.Env["ASPNETCORE_URLS"])
	}

	entry, ok := m.Entry()
	if !ok {
		t.Fatal("no entry service")
	}
	if entry.Name != "client" {
		t.Errorf("entry: got %q, want %q", entry.Name, "client")
	}
}

func TestParseAppliesDefaults(t *testing.T) {
	m, err := Parse([]byte(`services:
  app:
    command: "go run ./cmd/server"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Version != ManifestVersion {
		t.Errorf("version: got %d, want %d (omitted version defaults)", m.Version, ManifestVersion)
	}
	if got := m.Services[0].ReadyTimeout; got != DefaultReadyTimeout {
		t.Errorf("ready_timeout: got %s, want %s", got, DefaultReadyTimeout)
	}
	// A lone service is implicitly the entry point.
	if !m.Services[0].Entry {
		t.Error("the only service should be marked as entry")
	}
	entry, ok := m.Entry()
	if !ok || entry.Name != "app" {
		t.Errorf("Entry: got %q/%v, want app/true", entry.Name, ok)
	}
}

func TestParseInvalidManifests(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "duplicate service name",
			yaml: `services:
  web:
    command: "npm run dev"
  web:
    command: "npm run other"
`,
			wantErr: `duplicate service "web"`,
		},
		{
			// Both names fold to FORGE_PREVIEW_PORT_API_GATEWAY, so one
			// service would be told the other's port.
			name: "service names colliding in the port env var",
			yaml: `services:
  api-gateway:
    command: "go run ./cmd/api"
    entry: true
  API_gateway:
    command: "go run ./cmd/other"
`,
			wantErr: "both map to FORGE_PREVIEW_PORT_API_GATEWAY",
		},
		{
			name: "missing entry with two services",
			yaml: `services:
  api:
    command: "go run ./cmd/api"
  client:
    command: "npm run dev"
`,
			wantErr: "no service marked entry: true",
		},
		{
			name: "multiple entries",
			yaml: `services:
  api:
    command: "go run ./cmd/api"
    entry: true
  client:
    command: "npm run dev"
    entry: true
`,
			wantErr: "multiple services marked entry: true",
		},
		{
			name: "unknown service field",
			yaml: `services:
  api:
    comand: "go run ./cmd/api"
`,
			wantErr: `service "api": unknown field "comand"`,
		},
		{
			name: "unknown top-level field",
			yaml: `servcies:
  api:
    command: "go run ./cmd/api"
`,
			wantErr: "field servcies not found",
		},
		{
			name: "empty command",
			yaml: `services:
  api:
    command: "   "
`,
			wantErr: `service "api": command is required`,
		},
		{
			name: "no services",
			yaml: `version: 1
setup: "./setup.sh"
services: {}
`,
			wantErr: "no services declared",
		},
		{
			name: "unsupported version",
			yaml: `version: 2
services:
  api:
    command: "go run ./cmd/api"
`,
			wantErr: "unsupported version 2",
		},
		{
			name: "health without leading slash",
			yaml: `services:
  api:
    command: "go run ./cmd/api"
    health: "healthz"
`,
			wantErr: `service "api": health must be a path starting with "/"`,
		},
		{
			name: "bare number ready_timeout",
			yaml: `services:
  api:
    command: "go run ./cmd/api"
    ready_timeout: 120
`,
			wantErr: `ready_timeout must be a duration string such as "120s"`,
		},
		{
			name: "sub-second ready_timeout",
			yaml: `services:
  api:
    command: "go run ./cmd/api"
    ready_timeout: 500ms
`,
			wantErr: "ready_timeout must be at least 1s",
		},
		{
			name: "dir escaping the worktree",
			yaml: `services:
  api:
    command: "go run ./cmd/api"
    dir: "../elsewhere"
`,
			wantErr: "dir must stay inside the preview worktree",
		},
		{
			name: "absolute dir",
			yaml: `services:
  api:
    command: "go run ./cmd/api"
    dir: "/etc"
`,
			wantErr: "dir must be relative to the preview worktree",
		},
		{
			name: "invalid service name",
			yaml: `services:
  "my service":
    command: "go run ./cmd/api"
`,
			wantErr: "name must match",
		},
		{
			name: "services is a list",
			yaml: `services:
  - command: "go run ./cmd/api"
`,
			wantErr: "expected a mapping of service name to definition",
		},
		{
			name: "unknown service referenced in a template",
			yaml: `services:
  api:
    command: "go run ./cmd/api --port {{.Port}}"
    entry: true
  client:
    command: "npm run dev -- --api {{.ServicePort \"apo\"}}"
`,
			wantErr: `unknown service "apo"`,
		},
		{
			name: "broken template syntax",
			yaml: `services:
  api:
    command: "go run ./cmd/api --port {{.Port"
`,
			wantErr: "invalid template",
		},
		{
			name: "Port used in setup",
			yaml: `setup: "./setup.sh {{.Port}}"
services:
  api:
    command: "go run ./cmd/api"
`,
			wantErr: "{{.Port}} is only available inside a service",
		},
		{
			name:    "empty manifest",
			yaml:    "",
			wantErr: "manifest is empty",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	anvil := t.TempDir()
	writeManifest(t, anvil, twoServiceManifest)

	m, err := Load(anvil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := ManifestPath(anvil); m.Path != want {
		t.Errorf("Path: got %q, want %q", m.Path, want)
	}
	if len(m.Services) != 2 {
		t.Errorf("services: got %d, want 2", len(m.Services))
	}
	if !Exists(anvil) {
		t.Error("Exists should be true for an anvil with a manifest")
	}
}

func TestLoadNoManifest(t *testing.T) {
	anvil := t.TempDir()

	_, err := Load(anvil)
	if !errors.Is(err, ErrNoManifest) {
		t.Fatalf("expected ErrNoManifest, got %v", err)
	}
	if Exists(anvil) {
		t.Error("Exists should be false for an anvil without a manifest")
	}
}

func TestLoadInvalidManifestNamesTheFile(t *testing.T) {
	anvil := t.TempDir()
	writeManifest(t, anvil, `services:
  api:
    command: ""
`)

	_, err := Load(anvil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrNoManifest) {
		t.Fatalf("a broken manifest must not look like a missing one: %v", err)
	}
	if !strings.Contains(err.Error(), ManifestPath(anvil)) {
		t.Errorf("error %q should name the manifest path", err)
	}
}

func writeManifest(t *testing.T, anvil, content string) {
	t.Helper()
	dir := filepath.Join(anvil, ".forge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(content), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}
