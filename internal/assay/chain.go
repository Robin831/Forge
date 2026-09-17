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
// given run. A zero usage is still recorded against its kind only when the
// kind is already present: a rate-limited session billed nothing, and an entry
// for it would be a provider the run spent nothing on.
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
