package kiln

import (
	"strings"
	"testing"
)

func TestExpand(t *testing.T) {
	m, err := Parse([]byte(twoServiceManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	out, err := m.Expand(Context{
		PreviewID: "forge_abc1",
		Host:      "devbox.local",
		Ports:     map[string]int{"api": 42001, "client": 42002},
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}

	api, _ := out.Services.Get("api")
	if got, want := api.Env["ASPNETCORE_URLS"], "http://127.0.0.1:42001"; got != want {
		t.Errorf("api ASPNETCORE_URLS: got %q, want %q", got, want)
	}
	if got, want := api.Env["ConnectionStrings__Default"], "Server=localhost;Database=app_preview_forge_abc1"; got != want {
		t.Errorf("api connection string: got %q, want %q", got, want)
	}

	client, _ := out.Services.Get("client")
	if got, want := client.Command, "npm run dev -- --port 42002 --strictPort"; got != want {
		t.Errorf("client command: got %q, want %q", got, want)
	}
	// Cross-service port lookup: the client is told where the api listens.
	if got, want := client.Env["VITE_API_URL"], "http://devbox.local:42001"; got != want {
		t.Errorf("client VITE_API_URL: got %q, want %q", got, want)
	}

	// Non-templated fields survive untouched.
	if api.Health != "/healthz" || client.Dir != "client" {
		t.Errorf("non-template fields changed: health=%q dir=%q", api.Health, client.Dir)
	}
}

// TestExpandBindHost pins the reason {{.BindHost}} exists: a service that must
// be told what address to listen on gets preview_bind_host, not the public name
// a browser dials, so a manifest never has to hardcode either.
func TestExpandBindHost(t *testing.T) {
	m, err := Parse([]byte(`services:
  api:
    command: "dotnet run"
    env:
      ASPNETCORE_URLS: "http://{{.BindHost}}:{{.Port}}"
  client:
    command: "npm run dev -- --host {{.BindHost}} --port {{.Port}}"
    env:
      VITE_API_URL: "http://{{.Host}}:{{.ServicePort \"api\"}}"
    entry: true
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	out, err := m.Expand(Context{
		PreviewID: "forge_hz5w",
		Host:      "devbox.local",
		BindHost:  "0.0.0.0",
		Ports:     map[string]int{"api": 42001, "client": 42002},
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}

	api, _ := out.Services.Get("api")
	if got, want := api.Env["ASPNETCORE_URLS"], "http://0.0.0.0:42001"; got != want {
		t.Errorf("api ASPNETCORE_URLS: got %q, want %q", got, want)
	}

	client, _ := out.Services.Get("client")
	if got, want := client.Command, "npm run dev -- --host 0.0.0.0 --port 42002"; got != want {
		t.Errorf("client command: got %q, want %q", got, want)
	}
	// The URL handed to the browser still uses the public host — the two
	// variables must not collapse into one.
	if got, want := client.Env["VITE_API_URL"], "http://devbox.local:42001"; got != want {
		t.Errorf("client VITE_API_URL: got %q, want %q", got, want)
	}
}

// TestExpandBindHostInLifecycleCommands covers setup/teardown, where {{.Port}}
// is unavailable but {{.BindHost}} is not.
func TestExpandBindHostInLifecycleCommands(t *testing.T) {
	m, err := Parse([]byte(`setup: "./setup.sh {{.BindHost}}"
teardown: "./teardown.sh {{.BindHost}}"
services:
  api:
    command: "go run ./cmd/api --port {{.Port}}"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	out, err := m.Expand(Context{PreviewID: "p", Host: "devbox.local", BindHost: "127.0.0.1", Ports: map[string]int{"api": 42001}})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if got, want := out.Setup, "./setup.sh 127.0.0.1"; got != want {
		t.Errorf("setup: got %q, want %q", got, want)
	}
	if got, want := out.Teardown, "./teardown.sh 127.0.0.1"; got != want {
		t.Errorf("teardown: got %q, want %q", got, want)
	}
}

func TestExpandDoesNotMutateReceiver(t *testing.T) {
	m, err := Parse([]byte(twoServiceManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	before, _ := m.Services.Get("client")

	if _, err := m.Expand(Context{PreviewID: "p", Host: "h", Ports: map[string]int{"api": 1, "client": 2}}); err != nil {
		t.Fatalf("Expand: %v", err)
	}

	after, _ := m.Services.Get("client")
	if after.Command != before.Command {
		t.Errorf("command mutated: %q -> %q", before.Command, after.Command)
	}
	if !strings.Contains(after.Env["VITE_API_URL"], `{{.ServicePort "api"}}`) {
		t.Errorf("env mutated: %q", after.Env["VITE_API_URL"])
	}
}

func TestExpandSetupAndTeardown(t *testing.T) {
	m, err := Parse([]byte(`setup: "./setup.sh app_preview_{{.PreviewID}}"
teardown: "./teardown.sh app_preview_{{.PreviewID}} {{.ServicePort \"api\"}}"
services:
  api:
    command: "go run ./cmd/api --port {{.Port}}"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	out, err := m.Expand(Context{PreviewID: "forge_9epl", Host: "127.0.0.1", Ports: map[string]int{"api": 42010}})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if got, want := out.Setup, "./setup.sh app_preview_forge_9epl"; got != want {
		t.Errorf("setup: got %q, want %q", got, want)
	}
	if got, want := out.Teardown, "./teardown.sh app_preview_forge_9epl 42010"; got != want {
		t.Errorf("teardown: got %q, want %q", got, want)
	}
	if got, want := out.Services[0].Command, "go run ./cmd/api --port 42010"; got != want {
		t.Errorf("command: got %q, want %q", got, want)
	}
}

func TestExpandErrors(t *testing.T) {
	m, err := Parse([]byte(twoServiceManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	tests := []struct {
		name    string
		ctx     Context
		wantErr string
	}{
		{
			name:    "missing port for a service",
			ctx:     Context{PreviewID: "p", Host: "h", Ports: map[string]int{"client": 42002}},
			wantErr: `unknown service "api"`,
		},
		{
			name:    "zero port",
			ctx:     Context{PreviewID: "p", Host: "h", Ports: map[string]int{"api": 0, "client": 42002}},
			wantErr: `no port allocated for service "api"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.Expand(tc.ctx)
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestExpandErrorNamesTheField(t *testing.T) {
	m, err := Parse([]byte(twoServiceManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	_, err = m.Expand(Context{PreviewID: "p", Host: "h", Ports: map[string]int{"api": 42001, "client": 0}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), `service "client": command`) {
		t.Errorf("error %q should name the offending service and field", err)
	}
}

func TestExpandUnknownVariable(t *testing.T) {
	// Validation runs the same expansion, so an unknown variable is rejected
	// at parse time rather than when a preview is started.
	_, err := Parse([]byte(`services:
  api:
    command: "go run ./cmd/api --port {{.Prot}}"
`))
	if err == nil {
		t.Fatal("expected an error for an unknown template variable")
	}
	if !strings.Contains(err.Error(), "Prot") {
		t.Errorf("error %q should name the unknown variable", err)
	}
}
