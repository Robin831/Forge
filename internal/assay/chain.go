package assay

import (
	"context"

	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/provider"
)

// passProviderKey carries the provider a pass session must spawn on.
type passProviderKey struct{}

// withPassProvider returns ctx carrying the chain entry the next session of a
// pass is to run on. It rides on the context for the reason the turn budget
// does (withTurnBudget): PassRunner is the one seam every stub implements, and
// the chain walk in runDeepPass/runTriage is what decides which entry that is —
// the runner only spawns what it is handed.
func withPassProvider(ctx context.Context, pv provider.Provider) context.Context {
	return context.WithValue(ctx, passProviderKey{}, pv)
}

// PassProviderFrom reports the provider the chain walk handed this session,
// and false when none was (a runner called outside Review). It is exported so
// a custom PassRunner installed with WithRunner can honour the failover the
// engine decided on rather than resolving a provider of its own.
func PassProviderFrom(ctx context.Context) (provider.Provider, bool) {
	pv, ok := ctx.Value(passProviderKey{}).(provider.Provider)
	return pv, ok
}

// ReasonAuthFailed — the provider rejected the credentials. It is its own
// label rather than a flavour of ReasonProviderFailed because it is the one
// failure the chain walk must be told apart from a rate limit: a bad credential
// is not transient, and moving to the next provider on it would hide a broken
// configuration behind a pass that quietly ran somewhere else. The pass fails,
// the chain stops, and nothing retries it.
const ReasonAuthFailed = "auth_failed"

// failsOver reports whether a pass that ended on failure may move to the next
// provider in its chain. Only a rate limit does: it is the one failure that
// says nothing about the request and everything about the provider's capacity
// at that moment, which is exactly what another provider does not share. Every
// other ending stays where it is — an auth failure must surface rather than
// loop, a turn-budget or JSON failure has already had its in-provider retry, a
// cost stop would buy the same runaway elsewhere, and a success needs nothing.
func failsOver(f PassFailure) bool {
	return f.Reason == ReasonRateLimited
}

// ProviderUsage is one provider kind's share of a run's spend. A run is not
// one provider — each pass resolves its own chain, and a pass that failed over
// spent on two — so the cost tables' per-provider aggregate is written from
// this rather than from a whole-run attribution.
type ProviderUsage struct {
	// Provider is the provider kind ("claude", "copilot", "gemini").
	Provider string
	// Usage is everything sessions on that provider were billed.
	Usage cost.Usage
}

// addProviderUsage folds u into the entry for kind, appending one in
// first-seen order when there is none, so the list is deterministic for a
// given run. A zero usage never creates an entry for a kind not yet seen (a
// rate-limited session billed nothing).
func addProviderUsage(list []ProviderUsage, kind provider.Kind, u cost.Usage) []ProviderUsage {
	for i := range list {
		if list[i].Provider == string(kind) {
			list[i].Usage.Add(u)
			return list
		}
	}
	if u.IsZero() {
		return list
	}
	return append(list, ProviderUsage{Provider: string(kind), Usage: u})
}

// mergeProviderUsage folds every entry of b into a.
func mergeProviderUsage(a, b []ProviderUsage) []ProviderUsage {
	for _, e := range b {
		a = addProviderUsage(a, provider.Kind(e.Provider), e.Usage)
	}
	return a
}

// sessionModel is the model a pass recorded: the one the session reported
// (in-band, e.g. Claude's init event) where it did, else the model the chain
// entry configured. Empty means the provider ran its own default and said
// nothing about which it was.
func sessionModel(reported string, pv provider.Provider) string {
	if reported != "" {
		return reported
	}
	return pv.Model
}

// chainTelemetry is the part of a pass's outcome that a chain walk folds
// across the providers it tried. passResult and triageRun both embed it, and
// both walks (runDeepPass, runTriage) fold through the one method below, so a
// field that should accumulate across providers is added once and reaches both.
type chainTelemetry struct {
	// usage is the cumulative token accounting across every provider session
	// the pass made — the strict-JSON re-prompt and any turn-budget retry
	// included, and failed sessions along with successful ones, since the
	// provider bills all of them. The cache halves are summed for the same
	// reason the cost is: a pass that took a re-prompt or a retry really did
	// write the prefix more than once.
	usage cost.Usage
	// estCostUSD is costTracker's estimate of that same spend, summed over the
	// same sessions — the unit assay.max_cost_per_pass_usd is compared against.
	// Zero when no ceiling is configured or the backend streams no per-turn
	// usage, which are the cases where nothing measured it.
	estCostUSD float64
	// toolCalls is how many tool calls every session of the pass made together,
	// and filesRead how many distinct files they opened between them. Both are
	// cumulative, on usage's terms rather than turns': they measure how much
	// this pass explored, and exploration a re-prompt or a retry paid for was
	// still exploration. See PassReport.ToolCalls. filesRead is the file-shaped
	// part of the union (countFilesRead) rather than its length — the union
	// itself is the looser list the retry's diff scoping selects from.
	toolCalls int
	filesRead int
	// opened is the union of the files every session of the pass read, on
	// every provider it ran on. filesRead is its file-shaped count; the list
	// itself is kept so a failover can fold the next provider's reads into the
	// same union rather than adding two counts that may overlap.
	opened []string
	// provider is the chain entry the pass ENDED on — the one its recorded
	// outcome came from — and model the model that session reported (falling
	// back to the entry's configured one). Together they are what the pass
	// row persists, which is why they are the final provider's and not the
	// chain head's: a pass that failed over did not run on its head.
	provider provider.Provider
	model    string
	// failedOver reports that the pass moved past its chain's head because
	// an earlier entry was rate limited. It is what adds provider= to the
	// pass's segment of the telemetry line.
	failedOver bool
	// byProvider splits usage by the provider kind each session ran on. It
	// sums to usage: a pass that failed over was billed on both providers,
	// and the cost tables' per-provider aggregate must say so.
	byProvider []ProviderUsage
}

// fold adds step — what the pass did on chain entry i, pv — to the running
// totals, and takes the provider fields from it, since the entry folded last is
// the one the pass ended on.
func (t *chainTelemetry) fold(i int, pv provider.Provider, step chainTelemetry) {
	t.usage.Add(step.usage)
	t.estCostUSD += step.estCostUSD
	t.toolCalls += step.toolCalls
	t.opened = mergeOpenedFiles(t.opened, step.opened)
	t.filesRead = countFilesRead(t.opened)
	t.byProvider = addProviderUsage(t.byProvider, pv.Kind, step.usage)
	t.provider = pv
	t.model = sessionModel(step.model, pv)
	t.failedOver = i > 0
}
