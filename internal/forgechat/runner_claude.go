package forgechat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
)

// ClaudeRunner is the production Runner implementation. It spawns a
// short-lived claude (or fallback) session in a temp directory for each
// turn and parses the response into messages and stage transitions. The
// session does not run in any anvil worktree — the AI must not touch the
// filesystem during a Forge design conversation.
type ClaudeRunner struct {
	// Provider is the AI backend used for every turn. Callers typically
	// resolve this from settings.stage_providers["forgechat"] with a fallback
	// to the global provider chain.
	Provider provider.Provider
	// MaxTurns caps the AI session length. Defaults to 10. The grilling
	// prompt asks for a single JSON envelope, so a low budget is fine.
	MaxTurns int
	// Timeout caps the wall-clock duration of a single turn. Defaults to
	// 90 seconds. Kept short so the HTTP handler isn't held open forever.
	Timeout time.Duration
	// ExtraFlags are passed through to claude (e.g. --model).
	ExtraFlags []string
}

// NewClaudeRunner constructs a ClaudeRunner with sensible defaults.
func NewClaudeRunner(pv provider.Provider, extraFlags []string) *ClaudeRunner {
	return &ClaudeRunner{
		Provider:   pv,
		MaxTurns:   10,
		Timeout:    90 * time.Second,
		ExtraFlags: extraFlags,
	}
}

// Turn implements Runner.
func (r *ClaudeRunner) Turn(ctx context.Context, req TurnRequest) (*TurnResponse, error) {
	if r == nil {
		return nil, errors.New("forgechat: nil runner")
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	maxTurns := r.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 10
	}

	turnCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	workDir, err := os.MkdirTemp("", "forge-chat-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(workDir); rmErr != nil {
			// A leaked temp dir is not fatal to the turn (claude already
			// produced its output) but it can pile up under heavy use, so
			// surface it via the default logger rather than swallowing it.
			slog.Warn("forgechat: failed to remove temp work dir", "dir", workDir, "error", rmErr)
		}
	}()
	logDir := filepath.Join(workDir, "logs")

	prompt := BuildPrompt(req)
	flags := append([]string{"--max-turns", fmt.Sprintf("%d", maxTurns)}, r.ExtraFlags...)
	proc, err := smith.SpawnWithProvider(turnCtx, workDir, prompt, logDir, r.Provider, flags)
	if err != nil {
		return nil, fmt.Errorf("spawning %s session: %w", r.Provider.Label(), err)
	}

	res := proc.Wait()
	if res == nil {
		return nil, errors.New("forgechat: nil result from claude session")
	}
	if res.RateLimited {
		return nil, fmt.Errorf("forgechat: provider %s rate-limited", r.Provider.Label())
	}
	if res.ExitCode != 0 && res.FullOutput == "" {
		return nil, fmt.Errorf("forgechat: provider exited %d with no output: %s", res.ExitCode, truncate(res.ErrorOutput, 200))
	}

	output := res.FullOutput
	if output == "" {
		output = res.Output
	}

	return interpretResponse(req, output, res.CostUSD)
}

// interpretResponse converts raw claude output into a TurnResponse based on
// the request stage/mode. Pulled out so tests can drive the parser without
// spinning up the smith process.
func interpretResponse(req TurnRequest, output string, costUSD float64) (*TurnResponse, error) {
	resp := &TurnResponse{CostUSD: costUSD}

	switch req.Stage {
	case StageDrafting:
		body := stripFences(output)
		if body == "" {
			return nil, errors.New("forgechat: empty drafting response")
		}
		if req.Mode == ModePlan {
			resp.NewPlan = body
			resp.Messages = []EmittedMessage{{Kind: "plan", Content: body}}
		} else {
			resp.Messages = []EmittedMessage{{Kind: "text", Content: body}}
		}
	case StageGrilling:
		v, err := ParseGrillingResponse(output)
		if err != nil {
			return nil, fmt.Errorf("forgechat: could not parse grilling output: %w", err)
		}
		msgs, done := VerdictToMessages(v)
		resp.Messages = msgs
		if done {
			resp.NewStage = StageReady
		}
	default:
		return nil, fmt.Errorf("forgechat: unsupported stage %q", req.Stage)
	}

	return resp, nil
}

// truncate returns at most n runes of s, appending an ellipsis when the
// input was longer. Operates on runes (not bytes) so it never splits a
// multi-byte UTF-8 sequence — important because s often comes from claude
// stderr, which can contain non-ASCII content.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i] + "…"
		}
		count++
	}
	return s
}

// stripFences removes a single ```...``` wrapper if claude bracketed its
// reply in fences. Inner fences are left alone so nested code blocks keep
// rendering correctly.
func stripFences(s string) string {
	body := s
	body = trimSpaceLeftRight(body)
	if len(body) < 6 {
		return body
	}
	if body[:3] != "```" {
		return body
	}
	// Skip the opening fence line.
	for i := 3; i < len(body); i++ {
		if body[i] == '\n' {
			body = body[i+1:]
			break
		}
		if i == len(body)-1 {
			return s
		}
	}
	// Trim trailing closing fence if present.
	body = trimSpaceLeftRight(body)
	if len(body) >= 3 && body[len(body)-3:] == "```" {
		body = body[:len(body)-3]
	}
	return trimSpaceLeftRight(body)
}

func trimSpaceLeftRight(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\n' || s[j-1] == '\r') {
		j--
	}
	return s[i:j]
}
