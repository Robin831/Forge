// Package provider defines the AI provider abstraction for Forge.
//
// Forge supports multiple AI coding agents (Claude, Gemini, GitHub Copilot CLI)
// and selects among them in priority order.  When the active provider signals
// a rate limit the pipeline automatically retries with the next provider.
package provider

import (
	"context"
	"strings"
	"time"
)

// Kind is the canonical name of a provider.
type Kind string

const (
	Claude  Kind = "claude"
	Gemini  Kind = "gemini"
	Copilot Kind = "copilot" // gh copilot
)

// Quota holds remaining request/token counts and reset times.
type Quota struct {
	RequestsRemaining int        `json:"requests_remaining,omitempty"`
	RequestsLimit     int        `json:"requests_limit,omitempty"`
	RequestsReset     *time.Time `json:"requests_reset,omitempty"`
	TokensRemaining   int        `json:"tokens_remaining,omitempty"`
	TokensLimit       int        `json:"tokens_limit,omitempty"`
	TokensReset       *time.Time `json:"tokens_reset,omitempty"`
}

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
	// Model is the specific model to request from the provider.
	// Supported for all provider kinds via the --model flag.
	// When empty the provider's own default model is used.
	Model string
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

// Label returns a human-readable identifier for this provider suitable for
// log messages. When a model is set it returns "kind/model" (e.g.
// "gemini/gemini-2.5-pro"); otherwise it returns just the kind string.
func (p Provider) Label() string {
	if p.Model != "" {
		return string(p.Kind) + "/" + p.Model
	}
	return string(p.Kind)
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
// The prompt text is NOT included in the returned args — it must be written to
// the process's stdin instead.  This avoids the Windows CreateProcess
// command-line length limit (32 767 chars) for large Forge prompts.
//
// claudeFlags are additional flags specified by callers (e.g. --max-turns or
// --tools "").  For non-Claude providers these are translated where possible
// and silently dropped where they have no equivalent.
func (p Provider) BuildArgs(claudeFlags []string) []string {
	switch p.Kind {
	case Gemini:
		return p.geminiArgs(claudeFlags)
	case Copilot:
		return p.copilotArgs(claudeFlags)
	default:
		return p.claudeArgs(claudeFlags)
	}
}

func (p Provider) claudeArgs(extra []string) []string {
	// -p - tells Claude to read the prompt from stdin.
	base := []string{
		"--dangerously-skip-permissions",
		"-p", "-",
		"--output-format", "stream-json",
		"--verbose",
	}
	// Honour explicit model override from claude_flags; if none, use p.Model.
	model := p.Model
	var filtered []string
	for i := 0; i < len(extra); i++ {
		if extra[i] == "--model" && i+1 < len(extra) {
			model = extra[i+1]
			i++ // skip value
		} else {
			filtered = append(filtered, extra[i])
		}
	}
	if model != "" {
		base = append(base, "--model", model)
	}
	return append(base, filtered...)
}

func (p Provider) geminiArgs(claudeFlags []string) []string {
	// Gemini CLI: `gemini --yolo -o stream-json [--model <model>]`
	// Without a positional prompt argument the Gemini CLI reads the prompt from
	// stdin, avoiding the Windows command-line length limit.
	// --yolo is equivalent to claude's --dangerously-skip-permissions.
	// --output-format stream-json / -o stream-json enables machine-readable output.
	// --model selects a specific Gemini model; omitted when empty (CLI picks default).
	// Translate recognised claude flags; drop the rest.
	base := []string{
		"--yolo",
		"-o", "stream-json",
	}

	// Honour an explicit model override from claude_flags; otherwise use p.Model.
	model := p.Model
	i := 0
	for i < len(claudeFlags) {
		flag := claudeFlags[i]
		switch flag {
		case "--max-turns":
			i += 2 // skip value
		case "--tools":
			i += 2 // skip value — no direct Gemini equivalent
		case "--model":
			if i+1 < len(claudeFlags) {
				model = claudeFlags[i+1]
				i += 2
			} else {
				i++
			}
		default:
			i++
		}
	}

	if model != "" {
		base = append(base, "--model", model)
	}
	return base
}

func (p Provider) copilotArgs(claudeFlags []string) []string {
	// GitHub Copilot CLI:
	//   copilot -p - --yolo --silent --model claude-sonnet-4.6 --no-auto-update
	//
	// -p -       = read prompt from stdin (avoids Windows command-line length limit)
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
		"-p", "-",
		"--yolo",
		"--silent",
		"--model", model,
		"--no-auto-update",
	}
}

// Defaults returns the default ordered list of providers.
// Claude is tried first, then Gemini (no specific model — CLI picks its default).
//
// To use specific Gemini models in priority order, list them explicitly in
// forge.yaml using the "gemini/model-name" syntax:
//
//	settings:
//	  providers:
//	    - claude
//	    - gemini/gemini-2.5-pro
//	    - gemini/gemini-3-flash-preview
//	    - gemini/gemini-2.5-flash
//	    - gemini/gemini-2.5-flash-lite
//
// GitHub Copilot CLI is NOT included by default because it consumes
// premium "Copilot for CLI" quota that requires manual approval.
// To enable Copilot as a fallback, add it explicitly:
//
//	settings:
//	  providers: [claude, gemini, copilot]
func Defaults() []Provider {
	return []Provider{
		{Kind: Claude},
		{Kind: Gemini},
	}
}

// FromConfig builds a Provider slice from configuration strings.
//
// Accepted formats:
//
//	"claude"                          – Claude with default command
//	"gemini"                          – Gemini with default command and model
//	"gemini/gemini-2.5-pro"           – Gemini, specific model
//	"gemini:mybin"                     – Gemini, custom binary, default model
//	"gemini:mybin/gemini-2.5-pro"      – Gemini, custom binary, specific model
//
// The optional "/model" suffix selects a specific model to pass via --model.
// The optional ":command" infix overrides the binary to execute.
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
		// Split off optional "/model" suffix first (model names never contain '/').
		var model string
		if idx := strings.LastIndex(s, "/"); idx != -1 {
			model = s[idx+1:]
			s = s[:idx]
		}
		// Remaining: "kind" or "kind:command".
		parts := strings.SplitN(s, ":", 2)
		pv := Provider{Kind: Kind(strings.ToLower(parts[0])), Model: model}
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

// FetchQuota is a placeholder for future provider quota reporting via CLI.
//
// Neither 'claude --usage' nor 'gemini --quota' are supported non-interactive
// commands with a stable, machine-readable output format. Until a verified
// format is confirmed, this method is intentionally a no-op to avoid calling
// unsupported flags and silently swallowing real execution errors.
//
// Quota data is captured automatically from stream-json events during normal
// Smith runs (see readStreamJSON). FetchQuota exists as an extension point for
// any future proactive quota polling.
func (p Provider) FetchQuota(_ context.Context) (*Quota, error) {
	return nil, nil
}
