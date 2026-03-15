// Package provider defines the AI provider abstraction for Forge.
//
// Forge supports multiple AI coding agents (Claude, Gemini, GitHub Copilot CLI,
// OpenAI Codex CLI) and selects among them in priority order.  When the active provider signals
// a rate limit the pipeline automatically retries with the next provider.
package provider

import (
	"context"
	"maps"
	"strings"
	"time"
)

// Kind is the canonical name of a provider.
type Kind string

const (
	Claude  Kind = "claude"
	Gemini  Kind = "gemini"
	Copilot Kind = "copilot" // gh copilot
	OpenAI  Kind = "openai"  // OpenAI Codex CLI
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
	// Backend is a named backend configuration (e.g. "ollama") that sets
	// environment variables to redirect the provider CLI to an alternative
	// API endpoint. Empty means the provider's default backend.
	Backend string
	// Env holds additional environment variables to set on the subprocess.
	// These are merged on top of the inherited environment. Populated
	// automatically for known backends (e.g. ollama) or can be set manually.
	Env map[string]string
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
	case OpenAI:
		return "codex"
	default:
		return "claude"
	}
}

// Label returns a human-readable identifier for this provider suitable for
// log messages. When a backend is set it includes it (e.g. "claude:ollama/qwen2.5-coder:32b").
// When a model is set it returns "kind/model" (e.g. "gemini/gemini-2.5-pro");
// otherwise it returns just the kind string.
func (p Provider) Label() string {
	base := string(p.Kind)
	if p.Backend != "" {
		base += ":" + p.Backend
	}
	if p.Model != "" {
		return base + "/" + p.Model
	}
	return base
}

// Format returns the output format this provider writes to stdout.
func (p Provider) Format() OutputFormat {
	// All supported providers now emit structured JSONL (stream-json).
	// Copilot CLI supports --output-format json since mid-2025.
	return StreamJSON
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
	case OpenAI:
		return p.openaiArgs(claudeFlags)
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
	//   copilot -p - --yolo --output-format json --model claude-sonnet-4.6 --no-auto-update
	//
	// -p -              = read prompt from stdin (avoids Windows command-line length limit)
	// --yolo            = --allow-all-tools + --allow-all-paths + --allow-all-urls
	// --output-format json = emit JSONL events (enables token counting & cost estimation)
	// --model           = use Claude Sonnet 4.6 (best autonomous-coding model available)
	// --no-auto-update  = avoid interactive update prompts in CI/daemon context
	//
	// Unrecognised claude flags (--max-turns, --tools, --verbose, etc.) are dropped.
	model := p.Model
	if model == "" {
		model = "claude-sonnet-4.6"
	}

	// Allow callers to override the model via a --model flag in claudeFlags.
	for i := 0; i+1 < len(claudeFlags); i++ {
		if claudeFlags[i] == "--model" {
			model = claudeFlags[i+1]
			break
		}
	}

	// Copilot CLI uses dots in version numbers (claude-haiku-4.5) while the
	// Anthropic convention uses dashes (claude-haiku-4-5). Translate so users
	// can use either format in forge.yaml.
	model = copilotModelName(model)

	// Copilot CLI does NOT support "-p -" for stdin. Omitting -p entirely
	// makes Copilot read the prompt from piped stdin and run non-interactively
	// (equivalent to Claude's -p - behavior).
	return []string{
		"--yolo",
		"--output-format", "json",
		"--model", model,
		"--no-auto-update",
	}
}

// copilotModelName translates a model ID from Anthropic's dash convention
// (claude-haiku-4-5) to Copilot CLI's dot convention (claude-haiku-4.5).
// If the model already uses dots or isn't a recognized pattern, it's returned as-is.
func copilotModelName(model string) string {
	// Known prefixes for Claude models used in Copilot.
	prefixes := []string{
		"claude-sonnet-",
		"claude-haiku-",
		"claude-opus-",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(model, prefix) {
			version := model[len(prefix):]
			// Replace the first dash with a dot in the version part (e.g. "4-5" → "4.5").
			version = strings.Replace(version, "-", ".", 1)
			return prefix + version
		}
	}
	return model
}

func (p Provider) openaiArgs(claudeFlags []string) []string {
	// OpenAI Codex CLI:
	//   codex --full-auto --output-format stream-json [--model <model>]
	//
	// --full-auto     = autonomous mode (equivalent to --dangerously-skip-permissions)
	// --output-format stream-json = emit JSONL events for machine-readable output
	// --model         = select a specific model (e.g. gpt-5.1-codex, o3)
	//
	// Without a positional prompt argument the Codex CLI reads the prompt from
	// stdin, avoiding the Windows command-line length limit.
	// Unrecognised claude flags (--verbose, --tools, etc.) are dropped.
	base := []string{
		"--full-auto",
		"--output-format", "stream-json",
	}

	model := p.Model
	i := 0
	for i < len(claudeFlags) {
		flag := claudeFlags[i]
		switch flag {
		case "--max-turns":
			// Codex supports --max-turns natively.
			if i+1 < len(claudeFlags) {
				base = append(base, "--max-turns", claudeFlags[i+1])
				i += 2
			} else {
				i++
			}
		case "--tools":
			i += 2 // skip value — no direct Codex equivalent
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

// knownBackends maps backend names to the environment variables they inject.
// When a backend name appears in the ":backend" position of a config string,
// the Command field is left empty (using the Kind default) and the Env map
// is populated with these variables instead.
var knownBackends = map[string]map[string]string{
	"ollama": {
		"ANTHROPIC_BASE_URL":                      "http://localhost:11434",
		"ANTHROPIC_AUTH_TOKEN":                     "ollama",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
	},
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
//	"openai"                           – OpenAI Codex CLI with default command and model
//	"openai/o3"                        – OpenAI Codex CLI, specific model
//	"openai:codex/o3"                  – OpenAI, custom binary (codex), specific model
//	"claude:ollama"                    – Claude CLI targeting local Ollama instance
//	"claude:ollama/qwen2.5-coder:32b"  – Claude CLI targeting Ollama with specific model
//
// The optional "/model" suffix selects a specific model to pass via --model.
// The optional ":command" infix overrides the binary to execute, unless it
// matches a known backend name (e.g. "ollama"), in which case environment
// variables are set instead.
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
		// Remaining: "kind" or "kind:command" or "kind:backend".
		parts := strings.SplitN(s, ":", 2)
		pv := Provider{Kind: Kind(strings.ToLower(parts[0])), Model: model}
		if len(parts) == 2 {
			name := parts[1]
			if envVars, ok := knownBackends[strings.ToLower(name)]; ok {
				// Known backend — set env vars, keep default command.
				pv.Backend = strings.ToLower(name)
				pv.Env = make(map[string]string, len(envVars))
				maps.Copy(pv.Env, envVars)
			} else {
				// Custom binary override.
				pv.Command = name
			}
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
