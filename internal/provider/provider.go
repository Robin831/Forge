// Package provider defines the AI provider abstraction for Forge.
//
// Forge supports multiple AI coding agents (Claude, Gemini, GitHub Copilot CLI)
// and selects among them in priority order.  When the active provider signals
// a rate limit the pipeline automatically retries with the next provider.
package provider

import "strings"

// Kind is the canonical name of a provider.
type Kind string

const (
	Claude  Kind = "claude"
	Gemini  Kind = "gemini"
	Copilot Kind = "copilot" // gh copilot
)

// OutputFormat describes how the provider writes its response to stdout.
type OutputFormat int

const (
	// StreamJSON means the provider emits newline-delimited JSON events
	// (Claude and Gemini stream-json mode). smith.go parses these events.
	StreamJSON OutputFormat = iota
	// PlainText means the provider writes the raw assistant response text
	// directly to stdout (GitHub Copilot CLI --silent mode).
	PlainText
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
		return "copilot"
	default:
		return "claude"
	}
}

// Format returns the output format this provider writes to stdout.
func (p Provider) Format() OutputFormat {
	switch p.Kind {
	case Copilot:
		return PlainText
	default:
		return StreamJSON
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
		return p.copilotArgs(promptText, claudeFlags)
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
	// --yolo is equivalent to claude's --dangerously-skip-permissions.
	// --output-format stream-json / -o stream-json enables machine-readable output.
	// Translate recognised claude flags; drop the rest.
	base := []string{
		promptText, // positional argument (not -p)
		"--yolo",
		"-o", "stream-json",
	}

	i := 0
	for i < len(claudeFlags) {
		flag := claudeFlags[i]
		switch flag {
		case "--max-turns":
			i += 2 // skip value
		case "--tools":
			i += 2 // skip value — no direct Gemini equivalent
		default:
			i++
		}
	}

	return base
}

func (p Provider) copilotArgs(promptText string, claudeFlags []string) []string {
	// GitHub Copilot CLI:
	//   copilot -p "<prompt>" --yolo --silent --model claude-sonnet-4.6 --no-auto-update
	//
	// --yolo     = --allow-all-tools + --allow-all-paths + --allow-all-urls
	// --silent   = output only the agent response, no stats (plain text)
	// --model    = use Claude Sonnet 4.6 (best autonomous-coding model available)
	// --no-auto-update = avoid interactive update prompts in CI/daemon context
	//
	// Unrecognised claude flags (--max-turns, --tools, --verbose, etc.) are dropped.
	model := "claude-sonnet-4.6"

	// Allow callers to override the model via a --model flag in claudeFlags.
	for i := 0; i+1 < len(claudeFlags); i++ {
		if claudeFlags[i] == "--model" {
			model = claudeFlags[i+1]
			break
		}
	}

	return []string{
		"-p", promptText,
		"--yolo",
		"--silent",
		"--model", model,
		"--no-auto-update",
	}
}

// Defaults returns the default ordered list of providers.
// Claude is tried first, Gemini second, Copilot CLI third.
func Defaults() []Provider {
	return []Provider{
		{Kind: Claude},
		{Kind: Gemini},
		{Kind: Copilot},
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
		"you've hit",
		"hit your limit",
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
