// Package provider defines the AI provider abstraction for Forge.
//
// Forge supports multiple AI coding agents (Claude, Gemini, …) and selects
// among them in priority order.  When the active provider signals a rate limit
// the pipeline automatically retries with the next provider in the list.
package provider

import "strings"

// Kind is the canonical name of a provider.
type Kind string

const (
	Claude  Kind = "claude"
	Gemini  Kind = "gemini"
	Copilot Kind = "copilot" // gh copilot – placeholder, not yet implemented
)

// Provider describes one AI backend along with any per-instance overrides.
type Provider struct {
	// Kind is the canonical provider name.
	Kind Kind
	// Command is the binary to execute. If empty, the Kind default is used.
	Command string
}

// Cmd returns the effective binary name.
func (p Provider) Cmd() string {
	if p.Command != "" {
		return p.Command
	}
	switch p.Kind {
	case Gemini:
		return "gemini"
	case Copilot:
		return "gh"
	default:
		return "claude"
	}
}

// BuildArgs returns the full argument list for a one-shot non-interactive run.
//
// claudeFlags are additional flags specified by callers (e.g. --max-turns or
// --tools "").  For non-Claude providers these are translated where possible
// and silently dropped where they have no equivalent.
func (p Provider) BuildArgs(promptText string, claudeFlags []string) []string {
	switch p.Kind {
	case Gemini:
		return p.geminiArgs(promptText, claudeFlags)
	case Copilot:
		// gh copilot is interactive-only; fallback to best-effort text mode.
		return p.copilotArgs(promptText)
	default:
		return p.claudeArgs(promptText, claudeFlags)
	}
}

func (p Provider) claudeArgs(promptText string, extra []string) []string {
	base := []string{
		"--dangerously-skip-permissions",
		"-p", promptText,
		"--output-format", "stream-json",
		"--verbose",
	}
	return append(base, extra...)
}

func (p Provider) geminiArgs(promptText string, claudeFlags []string) []string {
	// Gemini CLI: `gemini <prompt> --yolo -o stream-json`
	// --yolo is equivalent to claude's --dangerously-skip-permissions. 	// --output-format stream-json / -o stream-json enables machine-readable output.
	// Translate recognised claude flags; drop the rest.
	base := []string{
		promptText,           // positional argument (not -p)
		"--yolo",
		"-o", "stream-json",
	}

	i := 0
	for i < len(claudeFlags) {
		flag := claudeFlags[i]
		switch flag {
		case "--max-turns":
			// Gemini one-shot runs don't need explicit turn limits; skip.
			i += 2
		case "--tools":
			// `--tools ""` → Gemini equivalent is omitting --allowed-tools which
			// defaults to no confirmation, but there's no "disable all" flag.
			// We intentionally skip to keep the run non-interactive.
			i += 2
		default:
			// Unknown claude flags are silently dropped.
			i++
		}
	}

	return base
}

func (p Provider) copilotArgs(promptText string) []string {
	// gh copilot suggest is conversational; best we can do is a one-shot suggest.
	return []string{"copilot", "suggest", "-t", "shell", promptText}
}

// Defaults returns the default ordered list of providers.
// Claude is always tried first; Gemini is the first fallback.
func Defaults() []Provider {
	return []Provider{
		{Kind: Claude},
		{Kind: Gemini},
	}
}

// FromConfig builds a Provider slice from configuration strings.
// Each string is a Kind or "kind:command" pair (e.g. "gemini" or "gemini:gemini2").
func FromConfig(specs []string) []Provider {
	if len(specs) == 0 {
		return Defaults()
	}
	out := make([]Provider, 0, len(specs))
	for _, s := range specs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		parts := strings.SplitN(s, ":", 2)
		pv := Provider{Kind: Kind(strings.ToLower(parts[0]))}
		if len(parts) == 2 {
			pv.Command = parts[1]
		}
		out = append(out, pv)
	}
	return out
}

// IsRateLimitError inspects exit code, stderr text, and the stream-json subtype
// to decide whether a Smith failure was caused by the provider's rate limit.
//
// This function is intentionally broad — it is better to fall back to the next
// provider unnecessarily than to hard-fail on a transient limit.
func IsRateLimitError(exitCode int, stderr, resultSubtype string) bool {
	// Claude emits a non-zero exit code on rate limit; check subtype first.
	if strings.Contains(strings.ToLower(resultSubtype), "rate_limit") ||
		strings.Contains(strings.ToLower(resultSubtype), "overloaded") {
		return true
	}

	// Common stderr patterns across providers.
	lower := strings.ToLower(stderr)
	rateLimitPhrases := []string{
		"rate limit",
		"ratelimit",
		"usage limit",
		"quota exceeded",
		"you have run out",
		"claude ai usage",
		"usage cap",
		"too many requests",
		"overloaded",
		"capacity",
		"plan limit",
		"hourly limit",
		"daily limit",
		"monthly limit",
	}
	for _, phrase := range rateLimitPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}

	// Exit code 2 is used by Claude CLI when the plan limit is hit.
	// (Exit code 1 is a general error; 2 is specifically rate-limit / auth.)
	if exitCode == 2 {
		return true
	}

	return false
}
