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

// defaultTurnTimeout is the wall-clock budget for a single forgechat turn
// when ClaudeRunner.Timeout is unset. Sized to cover a Claude session that
// actually does codebase grep/read work (the previous 90s fallback timed
// out before the agent had finished its first pass). Mirrored by
// config.ForgeChatSettings.TurnTimeout, which lets operators tune it from
// forge.yaml without recompiling.
const defaultTurnTimeout = 5 * time.Minute

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
	// defaultTurnTimeout (5m). Override at construction from
	// settings.forgechat.turn_timeout.
	Timeout time.Duration
	// ExtraFlags are passed through to claude (e.g. --model).
	ExtraFlags []string
}

// NewClaudeRunner constructs a ClaudeRunner with sensible defaults.
func NewClaudeRunner(pv provider.Provider, extraFlags []string) *ClaudeRunner {
	return &ClaudeRunner{
		Provider:   pv,
		MaxTurns:   10,
		Timeout:    defaultTurnTimeout,
		ExtraFlags: extraFlags,
	}
}

// turnStageLabel returns the human-readable stage label used in timeout
// logs. The four labels (drafter/grilling/plan/emit) match the user-facing
// modes so an operator reading the warning can map it back to what the
// session was doing.
func turnStageLabel(req TurnRequest) string {
	if req.Mode == ModeEmit {
		return "emit"
	}
	switch req.Stage {
	case StageGrilling:
		return "grilling"
	case StageDrafting:
		if req.Mode == ModePlan {
			return "plan"
		}
		return "drafter"
	default:
		return "drafter"
	}
}

// Turn implements Runner.
func (r *ClaudeRunner) Turn(ctx context.Context, req TurnRequest) (*TurnResponse, error) {
	if r == nil {
		return nil, errors.New("forgechat: nil runner")
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultTurnTimeout
	}
	maxTurns := r.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 10
	}

	turnCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	turnStart := time.Now()
	stage := turnStageLabel(req)

	// os.MkdirTemp("", ...) creates the directory under os.TempDir() (/tmp on
	// Linux), which is outside any git repository. smith.SpawnWithProvider calls
	// worktree.ValidateWorktreeDir, which allows directories that are not inside
	// any git repo — so this temp dir is safe without requiring a real worktree.
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
		if timedOut := timeoutResponse(turnCtx, req, stage, turnStart, timeout); timedOut != nil {
			return timedOut, nil
		}
		return nil, fmt.Errorf("spawning %s session: %w", r.Provider.Label(), err)
	}

	res := proc.Wait()
	if timedOut := timeoutResponse(turnCtx, req, stage, turnStart, timeout); timedOut != nil {
		// The context deadline fired during the session — return the sentinel
		// rather than parsing the truncated streamed preamble.
		return timedOut, nil
	}
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

// timeoutResponse returns the sentinel TurnResponse when turnCtx has hit
// its deadline, and nil otherwise. Centralising the check keeps the
// happy/sad paths in Turn easy to read and ensures the same warning fires
// for every exit point (spawn error, partial output, full Wait).
//
// The sentinel body reports the *actual elapsed time* (not the configured
// timeout) so that when the outer HTTP handler cancels before the runner's
// deadline — or when the deadline fires slightly off-budget — the user sees
// the wall-clock duration they actually waited rather than a misleading
// "after 5m0s" when only 2m elapsed.
func timeoutResponse(turnCtx context.Context, req TurnRequest, stage string, start time.Time, timeout time.Duration) *TurnResponse {
	if !errors.Is(turnCtx.Err(), context.DeadlineExceeded) {
		return nil
	}
	elapsed := time.Since(start).Round(time.Second)
	slog.Warn("forgechat: turn timed out",
		"session_id", req.SessionID,
		"turn_stage", stage,
		"elapsed", elapsed,
		"timeout", timeout,
	)
	body := fmt.Sprintf(
		"_(Drafter timed out after %s. Try a more focused follow-up question, or bump settings.forgechat.turn_timeout.)_",
		elapsed,
	)
	return &TurnResponse{
		Messages: []EmittedMessage{{Kind: "text", Content: body}},
	}
}

// interpretResponse converts raw claude output into a TurnResponse based on
// the request stage/mode. Pulled out so tests can drive the parser without
// spinning up the smith process.
func interpretResponse(req TurnRequest, output string, costUSD float64) (*TurnResponse, error) {
	resp := &TurnResponse{CostUSD: costUSD}

	// ModeEmit overrides the stage switch — emission is a one-shot turn
	// that runs while the session is in StageReady but does NOT advance any
	// further. The handler decides what to persist after materialising bd.
	if req.Mode == ModeEmit {
		env, err := ParseEmissionResponse(output)
		if err != nil {
			return nil, fmt.Errorf("forgechat: could not parse emission output: %w", err)
		}
		resp.Emission = env
		return resp, nil
	}

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
