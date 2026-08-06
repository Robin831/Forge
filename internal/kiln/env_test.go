package kiln

import (
	"reflect"
	"strings"
	"testing"
)

func TestSanitizePreviewID(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Forge-ir70", "forge_ir70"},
		{"forge/Forge-ir70", "forge_forge_ir70"},
		{"Hytte-9EPL", "hytte_9epl"},
		{"a--b__c", "a_b_c"},
		{"trailing-", "trailing"},
		{"-leading", "leading"},
		{"123", "p_123"},
		{"9lives", "p_9lives"},
		{"  spaced id  ", "spaced_id"},
		{"", "preview"},
		{"---", "preview"},
		{"drop table;--", "drop_table"},
	}
	for _, tc := range tests {
		if got := SanitizePreviewID(tc.in); got != tc.want {
			t.Errorf("SanitizePreviewID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizePreviewIDIsIdentifierSafe(t *testing.T) {
	for _, in := range []string{"Forge/ir-70", "feature/ABC.1", "42", "ünïcode"} {
		got := SanitizePreviewID(in)
		if got == "" {
			t.Fatalf("SanitizePreviewID(%q) is empty", in)
		}
		first := got[0]
		if !(first >= 'a' && first <= 'z') {
			t.Errorf("SanitizePreviewID(%q) = %q does not start with a letter", in, got)
		}
		for i := 0; i < len(got); i++ {
			c := got[i]
			if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '_' {
				t.Errorf("SanitizePreviewID(%q) = %q contains unsafe byte %q", in, got, c)
			}
		}
	}
}

func TestPortEnvVar(t *testing.T) {
	tests := map[string]string{
		"api":         "FORGE_PREVIEW_PORT_API",
		"api-gateway": "FORGE_PREVIEW_PORT_API_GATEWAY",
		"Client":      "FORGE_PREVIEW_PORT_CLIENT",
	}
	for in, want := range tests {
		if got := PortEnvVar(in); got != want {
			t.Errorf("PortEnvVar(%q) = %q, want %q", in, got, want)
		}
	}
}

func testEnvContext() PreviewEnv {
	return PreviewEnv{
		PreviewID:    "forge_ir70",
		BeadID:       "Forge-ir70",
		Branch:       "forge/Forge-ir70",
		WorktreePath: "/anvil/.previews/Forge-ir70",
		AnvilName:    "forge",
		AnvilPath:    "/anvil",
		Ports:        map[string]int{"api": 42001, "client": 42002},
	}
}

func envMap(t *testing.T, environ []string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(environ))
	for _, entry := range environ {
		key, value, found := strings.Cut(entry, "=")
		if !found {
			t.Fatalf("malformed environment entry %q", entry)
		}
		if _, dup := out[key]; dup {
			t.Errorf("environment has duplicate key %q", key)
		}
		out[key] = value
	}
	return out
}

func TestBuildEnvInjectsForgeContext(t *testing.T) {
	env := BuildEnv([]string{"PATH=/usr/bin", "HOME=/home/forge"}, testEnvContext(), nil)
	got := envMap(t, env)

	want := map[string]string{
		"PATH":                      "/usr/bin",
		"HOME":                      "/home/forge",
		"FORGE_PREVIEW_ID":          "forge_ir70",
		"FORGE_BEAD_ID":             "Forge-ir70",
		"FORGE_BRANCH":              "forge/Forge-ir70",
		"FORGE_WORKTREE_PATH":       "/anvil/.previews/Forge-ir70",
		"FORGE_ANVIL_NAME":          "forge",
		"FORGE_ANVIL_PATH":          "/anvil",
		"FORGE_PREVIEW_PORT_API":    "42001",
		"FORGE_PREVIEW_PORT_CLIENT": "42002",
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %q, want %q", key, got[key], value)
		}
	}
	if len(got) != len(want) {
		t.Errorf("environment has %d entries, want %d: %v", len(got), len(want), env)
	}
}

func TestBuildEnvLayering(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"ASPNETCORE_URLS=http://inherited",
		// A stale worker context must not leak into the preview.
		"FORGE_BEAD_ID=Forge-someone-else",
		"FORGE_WORKTREE_PATH=/anvil/.workers/other",
	}
	serviceEnv := map[string]string{
		"ASPNETCORE_URLS":  "http://127.0.0.1:42001",
		"FORGE_PREVIEW_ID": "manifest-tries-to-lie",
	}

	got := envMap(t, BuildEnv(base, testEnvContext(), serviceEnv))

	if got["ASPNETCORE_URLS"] != "http://127.0.0.1:42001" {
		t.Errorf("manifest env did not override the inherited value: %q", got["ASPNETCORE_URLS"])
	}
	if got["FORGE_PREVIEW_ID"] != "forge_ir70" {
		t.Errorf("manifest overrode the injected FORGE_PREVIEW_ID: %q", got["FORGE_PREVIEW_ID"])
	}
	if got["FORGE_BEAD_ID"] != "Forge-ir70" {
		t.Errorf("inherited FORGE_BEAD_ID leaked: %q", got["FORGE_BEAD_ID"])
	}
	if got["FORGE_WORKTREE_PATH"] != "/anvil/.previews/Forge-ir70" {
		t.Errorf("inherited FORGE_WORKTREE_PATH leaked: %q", got["FORGE_WORKTREE_PATH"])
	}
}

func TestBuildEnvIsDeterministic(t *testing.T) {
	base := []string{"PATH=/usr/bin"}
	svc := map[string]string{"B": "2", "A": "1", "C": "3"}
	first := BuildEnv(base, testEnvContext(), svc)
	second := BuildEnv(base, testEnvContext(), svc)
	if !reflect.DeepEqual(first, second) {
		t.Errorf("BuildEnv is not deterministic:\n%v\n%v", first, second)
	}
}

func TestPreviewEnvEnvironIncludesEveryServicePort(t *testing.T) {
	environ := testEnvContext().Environ()
	got := envMap(t, environ)
	for _, key := range []string{"FORGE_PREVIEW_PORT_API", "FORGE_PREVIEW_PORT_CLIENT"} {
		if got[key] == "" {
			t.Errorf("%s missing from %v", key, environ)
		}
	}
}
