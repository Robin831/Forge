package web

import (
	"net"
	"strings"

	"golang.org/x/net/publicsuffix"

	"github.com/Robin831/Forge/internal/kiln"
)

// sharedCookieDomain reports the domain the Hearth session cookie must be
// scoped to for a browser to also send it to preview hostnames under
// proxyBase — the primary half of the preview auth gate.
//
// A cookie with no Domain attribute is host-only: issued on
// hearth.example.com it is never sent to forge-a1b2.preview.example.com, so
// the proxy would see no session at all. Widening Domain to the suffix the two
// share fixes that in the deployment where they do share one, which is the
// common case (a wildcard record next to the dashboard's own).
//
// The check is deliberately conservative — a cookie Domain is a grant, and the
// cost of getting it wrong is handing the session to every host under the
// suffix:
//
//   - either name empty, an IP literal, or a single label ("localhost") → no.
//     There is no registrable parent to widen to, and browsers reject a Domain
//     on those anyway.
//   - no dot-aligned suffix in common → no. Unrelated names must not share.
//   - the shared suffix is at or above the public suffix ("github.io" for
//     a.github.io + b.github.io, "example.co.uk" is fine but "co.uk" is not)
//     → no. That is the supercookie case: the grant would cross an
//     administrative boundary to hosts nobody here controls.
//
// The returned domain is bare (no leading dot). Go writes Domain=example.com
// and browsers treat that as "this host and its subdomains" — the leading dot
// is a pre-RFC-6265 spelling of the same thing.
//
// When ok is false the caller must not widen the cookie; the token exchange in
// previewtoken.go is what covers that deployment instead.
func sharedCookieDomain(hearthHost, proxyBase string) (string, bool) {
	hearth := normalizeCookieHost(hearthHost)
	base := normalizeCookieHost(proxyBase)
	if hearth == "" || base == "" {
		return "", false
	}
	if isIPHost(hearth) || isIPHost(base) {
		return "", false
	}

	shared := commonDomainSuffix(hearth, base)
	if shared == "" || !strings.Contains(shared, ".") {
		// Single-label or no overlap: nothing registrable to scope to.
		return "", false
	}
	// EffectiveTLDPlusOne fails for a name that *is* a public suffix, and
	// returns the registrable domain otherwise. Requiring the shared suffix to
	// sit at or below that domain is what rejects "github.io" while accepting
	// "example.com" and "team.example.co.uk".
	registrable, err := publicsuffix.EffectiveTLDPlusOne(shared)
	if err != nil || registrable == "" {
		return "", false
	}
	if shared != registrable && !strings.HasSuffix(shared, "."+registrable) {
		return "", false
	}
	return shared, true
}

// commonDomainSuffix returns the longest suffix a and b share on a label
// boundary, or "" when they share none. Equal hosts share the whole name.
func commonDomainSuffix(a, b string) string {
	aLabels := strings.Split(a, ".")
	bLabels := strings.Split(b, ".")
	n := 0
	for n < len(aLabels) && n < len(bLabels) {
		if aLabels[len(aLabels)-1-n] != bLabels[len(bLabels)-1-n] {
			break
		}
		n++
	}
	if n == 0 {
		return ""
	}
	return strings.Join(aLabels[len(aLabels)-n:], ".")
}

// normalizeCookieHost reduces a Host header or a configured base to the bare
// lowercase hostname both sides are compared in: no port, no trailing root
// dot, no surrounding whitespace.
func normalizeCookieHost(host string) string {
	return kiln.NormalizeHostname(stripHostPort(strings.TrimSpace(host)))
}

// stripHostPort drops a ":port" suffix. A bare host makes SplitHostPort fail,
// in which case the input is already what we want. A bracketed IPv6 literal
// loses its brackets here, which is fine: the only caller rejects IP hosts.
func stripHostPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return strings.Trim(host, "[]")
}

// isIPHost reports whether host is an IP literal rather than a DNS name.
// Cookies cannot carry a Domain for an IP, and 127.0.0.1 shares no suffix with
// anything, so both halves of a Domain decision must reject them.
func isIPHost(host string) bool {
	return net.ParseIP(host) != nil
}
