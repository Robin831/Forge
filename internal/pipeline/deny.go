package pipeline

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
)

// DenyViolation describes a single deny pattern match.
type DenyViolation struct {
	Kind    string // "file" or "command"
	Pattern string // the deny pattern that matched
	Value   string // the file path or command that triggered the match
}

func (v DenyViolation) String() string {
	return fmt.Sprintf("[%s] pattern %q matched %q", v.Kind, v.Pattern, v.Value)
}

// checkDenyPatterns validates the Smith output against per-anvil deny patterns.
// It checks changed files in the diff against file deny patterns, and bash
// commands in the Smith output against command deny patterns.
// Returns a list of violations (empty if none).
func checkDenyPatterns(worktreePath, preSmithSHA, smithOutput string, cfg *config.DenyPatternsConfig) []DenyViolation {
	if cfg == nil {
		return nil
	}

	var violations []DenyViolation

	if len(cfg.Files) > 0 {
		violations = append(violations, checkFileDenyPatterns(worktreePath, preSmithSHA, cfg.Files)...)
	}

	if len(cfg.Commands) > 0 {
		violations = append(violations, checkCommandDenyPatterns(smithOutput, cfg.Commands)...)
	}

	return violations
}

// checkFileDenyPatterns gets changed file paths from the diff and matches
// them against the deny glob patterns.
func checkFileDenyPatterns(worktreePath, preSmithSHA string, patterns []string) []DenyViolation {
	files := gitDiffNameOnly(worktreePath, preSmithSHA)
	if len(files) == 0 {
		return nil
	}

	var violations []DenyViolation
	for _, file := range files {
		for _, pattern := range patterns {
			if matchDenyPattern(file, pattern) {
				violations = append(violations, DenyViolation{
					Kind:    "file",
					Pattern: pattern,
					Value:   file,
				})
				break // one violation per file is enough
			}
		}
	}
	return violations
}

// checkCommandDenyPatterns scans the raw Smith output (stream JSON lines)
// for bash tool_use invocations and matches commands against deny patterns.
func checkCommandDenyPatterns(smithOutput string, patterns []string) []DenyViolation {
	commands := extractBashCommands(smithOutput)
	if len(commands) == 0 {
		return nil
	}

	seen := make(map[string]bool) // deduplicate violations
	var violations []DenyViolation
	for _, cmd := range commands {
		for _, pattern := range patterns {
			if matchDenyPattern(cmd, pattern) {
				key := pattern + "\x00" + cmd
				if !seen[key] {
					seen[key] = true
					violations = append(violations, DenyViolation{
						Kind:    "command",
						Pattern: pattern,
						Value:   cmd,
					})
				}
				break
			}
		}
	}
	return violations
}

// gitDiffNameOnly returns the list of changed file paths between preSmithSHA
// and the current worktree state.
func gitDiffNameOnly(worktreePath, preSmithSHA string) []string {
	if preSmithSHA == "" {
		return nil
	}
	cmd := executil.HideWindow(exec.Command("git", "-C", worktreePath, "diff", "--name-only", preSmithSHA))
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files
}

// matchDenyPattern matches a value against a deny glob pattern.
// The pattern is matched against both the full path and the basename to
// support patterns like "*.env" matching "config/.env" as well as ".env".
// Patterns containing "/" are matched only against the full path.
// A trailing "*" acts as a prefix match (e.g. "git push --force*" matches
// "git push --force-with-lease").
func matchDenyPattern(value, pattern string) bool {
	// Try direct filepath.Match against the full value.
	if matched, _ := filepath.Match(pattern, value); matched {
		return true
	}

	// For file patterns without a directory separator, also try matching
	// against just the basename so "*.env" matches "config/.env".
	if !strings.Contains(pattern, "/") {
		base := filepath.Base(value)
		if matched, _ := filepath.Match(pattern, base); matched {
			return true
		}
	}

	// For patterns with a directory separator, try matching against each
	// suffix of the path so ".forge/*" matches "src/.forge/config.yaml".
	if strings.Contains(pattern, "/") {
		parts := strings.Split(value, "/")
		for i := range parts {
			subpath := strings.Join(parts[i:], "/")
			if matched, _ := filepath.Match(pattern, subpath); matched {
				return true
			}
		}
	}

	return false
}

// extractBashCommands parses Smith's stream-json output for bash tool_use
// invocations and returns the command strings.
func extractBashCommands(output string) []string {
	var commands []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Parse as a stream event to find assistant messages with tool_use blocks.
		var event struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message,omitempty"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		if event.Type != "assistant" || len(event.Message) == 0 {
			continue
		}

		var msg struct {
			Content []struct {
				Type  string `json:"type"`
				Name  string `json:"name"`
				Input struct {
					Command string `json:"command"`
				} `json:"input,omitempty"`
			} `json:"content"`
		}
		if err := json.Unmarshal(event.Message, &msg); err != nil {
			continue
		}

		for _, block := range msg.Content {
			if block.Type == "tool_use" && isBashTool(block.Name) && block.Input.Command != "" {
				commands = append(commands, block.Input.Command)
			}
		}
	}
	return commands
}

// isBashTool returns true if the tool name represents a bash/shell execution tool.
func isBashTool(name string) bool {
	switch strings.ToLower(name) {
	case "bash", "shell", "execute", "run":
		return true
	}
	return false
}

// formatDenyViolations produces a human-readable summary of deny pattern violations.
func formatDenyViolations(violations []DenyViolation) string {
	var b strings.Builder
	b.WriteString("Smith deny pattern violations detected:\n")
	for _, v := range violations {
		fmt.Fprintf(&b, "  - %s\n", v.String())
	}
	return b.String()
}
