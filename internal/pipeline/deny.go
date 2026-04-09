package pipeline

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path"
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
func checkDenyPatterns(worktreePath, preSmithSHA, smithOutput string, cfg *config.DenyPatternsConfig) ([]DenyViolation, error) {
	if cfg == nil {
		return nil, nil
	}

	var violations []DenyViolation

	if len(cfg.Files) > 0 {
		fileViolations, err := checkFileDenyPatterns(worktreePath, preSmithSHA, cfg.Files)
		if err != nil {
			return nil, err
		}
		violations = append(violations, fileViolations...)
	}

	if len(cfg.Commands) > 0 {
		violations = append(violations, checkCommandDenyPatterns(smithOutput, cfg.Commands)...)
	}

	return violations, nil
}

// checkFileDenyPatterns gets changed file paths from the diff and matches
// them against the deny glob patterns.
func checkFileDenyPatterns(worktreePath, preSmithSHA string, patterns []string) ([]DenyViolation, error) {
	files, err := gitDiffNameOnly(worktreePath, preSmithSHA)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}

	var violations []DenyViolation
	for _, file := range files {
		for _, pattern := range patterns {
			if matchFileDenyPattern(file, pattern) {
				violations = append(violations, DenyViolation{
					Kind:    "file",
					Pattern: pattern,
					Value:   file,
				})
				break // one violation per file is enough
			}
		}
	}
	return violations, nil
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
			if matchCommandPattern(cmd, pattern) {
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
func gitDiffNameOnly(worktreePath, preSmithSHA string) ([]string, error) {
	if preSmithSHA == "" {
		return nil, nil
	}
	cmd := executil.HideWindow(exec.Command("git", "-C", worktreePath, "diff", "--name-only", preSmithSHA))
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff --name-only: %w", err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// matchFileDenyPattern matches a git file path (always slash-separated) against
// a deny glob pattern. Uses path.Match (not filepath.Match) so behaviour is
// consistent across platforms regardless of the OS path separator.
//
//   - Patterns without "/" are also matched against the basename so that
//     "*.env" matches "config/.env".
//   - Patterns with "/" are additionally matched against each suffix of the
//     path so that ".forge/*" matches "src/.forge/config.yaml".
func matchFileDenyPattern(value, pattern string) bool {
	// git always uses forward slashes; normalise just in case.
	value = strings.ReplaceAll(value, "\\", "/")

	// Direct match against the full path.
	if matched, _ := path.Match(pattern, value); matched {
		return true
	}

	// For patterns without a directory separator, also match the basename.
	if !strings.Contains(pattern, "/") {
		base := path.Base(value)
		if matched, _ := path.Match(pattern, base); matched {
			return true
		}
	}

	// For patterns with a directory separator, match against each suffix.
	if strings.Contains(pattern, "/") {
		parts := strings.Split(value, "/")
		for i := range parts {
			subpath := strings.Join(parts[i:], "/")
			if matched, _ := path.Match(pattern, subpath); matched {
				return true
			}
		}
	}

	return false
}

// matchCommandPattern matches a shell command string against a deny glob
// pattern. Unlike file patterns, '/' has no special meaning here — '*' matches
// any sequence of characters including '/'. This lets patterns like
// "rm -rf /*" or "/usr/bin/*" work as users would expect.
func matchCommandPattern(cmd, pattern string) bool {
	return matchFlatGlob(pattern, cmd)
}

// matchFlatGlob is a simple glob where '?' matches one character and '*'
// matches any sequence of characters (including '/'). It does NOT treat '/'
// as a path separator, making it suitable for matching arbitrary strings such
// as shell command lines.
func matchFlatGlob(pattern, s string) bool {
	for {
		if len(pattern) == 0 {
			return len(s) == 0
		}
		if pattern[0] == '*' {
			// Consume consecutive stars.
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if len(pattern) == 0 {
				return true
			}
			// Try to match the rest of the pattern at every position in s.
			for i := 0; i <= len(s); i++ {
				if matchFlatGlob(pattern, s[i:]) {
					return true
				}
			}
			return false
		}
		if len(s) == 0 {
			return false
		}
		if pattern[0] == '?' || pattern[0] == s[0] {
			pattern = pattern[1:]
			s = s[1:]
		} else {
			return false
		}
	}
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
