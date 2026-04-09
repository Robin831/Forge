package pipeline

import (
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/config"
)

func TestMatchFileDenyPattern(t *testing.T) {
	tests := []struct {
		value   string
		pattern string
		want    bool
	}{
		// Exact match
		{".env", ".env", true},
		// Wildcard extension
		{"secrets.env", "*.env", true},
		{"config/.env", "*.env", true},
		{".env.local", ".env.*", true},
		// Key/PEM files nested in directories
		{"certs/server.key", "*.key", true},
		{"deep/path/to/cert.pem", "*.pem", true},
		// Directory pattern
		{".forge/config.yaml", ".forge/*", true},
		{"src/.forge/rules.yaml", ".forge/*", true},
		// No match
		{"main.go", "*.env", false},
		{"README.md", "*.key", false},
		{"forge/config.yaml", ".forge/*", false},
		// Pattern with no wildcard, no match
		{"other.txt", "secret.txt", false},
		{"secret.txt", "secret.txt", true},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_vs_"+tt.value, func(t *testing.T) {
			got := matchFileDenyPattern(tt.value, tt.pattern)
			if got != tt.want {
				t.Errorf("matchFileDenyPattern(%q, %q) = %v, want %v", tt.value, tt.pattern, got, tt.want)
			}
		})
	}
}

func TestMatchCommandPattern(t *testing.T) {
	tests := []struct {
		cmd     string
		pattern string
		want    bool
	}{
		// Exact match
		{"rm -rf /", "rm -rf /", true},
		// Trailing wildcard
		{"git push --force", "git push --force*", true},
		{"git push --force-with-lease", "git push --force*", true},
		{"git push origin main", "git push --force*", false},
		// Wildcard matches across '/' (unlike filepath.Match)
		{"rm -rf /home/user", "rm -rf /*", true},
		{"rm -rf /usr/local/bin", "rm -rf /*", true},
		{"/usr/bin/python3", "/usr/bin/*", true},
		{"/usr/bin/python3", "/bin/*", false},
		// No wildcard
		{"other", "secret", false},
		{"secret", "secret", true},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_vs_"+tt.cmd, func(t *testing.T) {
			got := matchCommandPattern(tt.cmd, tt.pattern)
			if got != tt.want {
				t.Errorf("matchCommandPattern(%q, %q) = %v, want %v", tt.cmd, tt.pattern, got, tt.want)
			}
		})
	}
}

func TestExtractBashCommands(t *testing.T) {
	// Simulate stream-json output with assistant tool_use events.
	output := `{"type":"assistant","message":{"content":[{"type":"text","text":"Let me check the code."}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"path":"main.go"}}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"rm -rf /tmp/test"}}]}}
not json at all
{"type":"result","subtype":"success"}
`
	commands := extractBashCommands(output)
	if len(commands) != 2 {
		t.Fatalf("expected 2 commands, got %d: %v", len(commands), commands)
	}
	if commands[0] != "go test ./..." {
		t.Errorf("command[0] = %q, want %q", commands[0], "go test ./...")
	}
	if commands[1] != "rm -rf /tmp/test" {
		t.Errorf("command[1] = %q, want %q", commands[1], "rm -rf /tmp/test")
	}
}

func TestExtractBashCommands_Empty(t *testing.T) {
	commands := extractBashCommands("")
	if len(commands) != 0 {
		t.Fatalf("expected 0 commands, got %d", len(commands))
	}
}

func TestCheckCommandDenyPatterns(t *testing.T) {
	output := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git push --force origin main"}}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}
`
	patterns := []string{"git push --force*", "rm -rf /"}
	violations := checkCommandDenyPatterns(output, patterns)
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d: %v", len(violations), violations)
	}
	if violations[0].Kind != "command" {
		t.Errorf("kind = %q, want %q", violations[0].Kind, "command")
	}
	if violations[0].Pattern != "git push --force*" {
		t.Errorf("pattern = %q, want %q", violations[0].Pattern, "git push --force*")
	}
}

func TestCheckDenyPatterns_NilConfig(t *testing.T) {
	violations, err := checkDenyPatterns("/tmp", "abc123", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected 0 violations for nil config, got %d", len(violations))
	}
}

func TestCheckDenyPatterns_EmptyPatterns(t *testing.T) {
	cfg := &config.DenyPatternsConfig{}
	violations, err := checkDenyPatterns("/tmp", "abc123", "", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected 0 violations for empty patterns, got %d", len(violations))
	}
}

func TestFormatDenyViolations(t *testing.T) {
	violations := []DenyViolation{
		{Kind: "file", Pattern: "*.env", Value: ".env"},
		{Kind: "command", Pattern: "rm -rf /", Value: "rm -rf /"},
	}
	result := formatDenyViolations(violations)
	if result == "" {
		t.Error("expected non-empty result")
	}
	if !strings.Contains(result, "*.env") || !strings.Contains(result, ".env") {
		t.Errorf("missing file violation in output: %s", result)
	}
	if !strings.Contains(result, "rm -rf /") {
		t.Errorf("missing command violation in output: %s", result)
	}
}

func TestIsBashTool(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"Bash", true},
		{"bash", true},
		{"Shell", true},
		{"Execute", true},
		{"Run", true},
		{"Read", false},
		{"Write", false},
		{"Grep", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBashTool(tt.name); got != tt.want {
				t.Errorf("isBashTool(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
