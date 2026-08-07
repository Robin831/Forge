package kiln

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
)

// maxDNSLabel is the longest a single DNS label may be (RFC 1035).
const maxDNSLabel = 63

// ErrPreviewLabelCollision is what a start refused because two bead ids fold to
// the same host label wraps. Callers use errors.Is to tell "this bead cannot be
// routed while that one is up" apart from a broken preview.
var ErrPreviewLabelCollision = errors.New("kiln: preview host label collision")

// PreviewLabelCollisionError reports bead ids that share one host label. Under
// settings.preview_proxy_base the label *is* the address — two previews sharing
// it would be one preview as far as the proxy is concerned, so the collision is
// reported when the second preview starts rather than when a request arrives
// and silently lands on the wrong branch.
type PreviewLabelCollisionError struct {
	// Label is the DNS label the bead ids collide on.
	Label string
	// BeadIDs are the colliding ids, sorted, so the message is deterministic.
	BeadIDs []string
}

func (e *PreviewLabelCollisionError) Error() string {
	return fmt.Sprintf("kiln: bead ids %s all map to the preview host label %q — settings.preview_proxy_base routes by that label, so only one of them can be previewed at a time",
		quoteAll(e.BeadIDs), e.Label)
}

// Unwrap makes errors.Is(err, ErrPreviewLabelCollision) work.
func (e *PreviewLabelCollisionError) Unwrap() error { return ErrPreviewLabelCollision }

// PreviewLabel reduces a bead id to a DNS label — the leftmost component of a
// preview hostname under settings.preview_proxy_base (`<label>.<base>`).
//
// It is SanitizePreviewID with '_' folded to '-'. SanitizePreviewID already
// guarantees `[a-z][a-z0-9_]*` with no leading, trailing or doubled '_', and
// '_' is the one character in that alphabet a hostname may not carry, so the
// fold is the whole transform: the result is `[a-z][a-z0-9-]*` with no leading,
// trailing or doubled '-'. That last property is what keeps `--` free as the
// label/service separator ParsePreviewHost splits on.
//
// The mapping is deliberately *not* injective: "Forge-ab1" and "Forge_ab1" both
// yield "forge-ab1". Nothing here can prevent that — see
// CheckPreviewLabelCollisions, which catches it at preview start.
//
// A bead id longer than 63 characters yields an over-long label. Bead ids are
// short by construction, so this is documented rather than truncated: silently
// shortening would turn a routable name into a second collision source.
func PreviewLabel(beadID string) string {
	return strings.ReplaceAll(SanitizePreviewID(beadID), "_", "-")
}

// ServiceLabel renders a manifest service name as the DNS label it is addressed
// by in the `<label>--<service>.<base>` form: lowercased, with '.' and '_'
// folded to '-'. It is the inverse ParsePreviewHost's service half is matched
// against, and what EntryURL builds a named-service link with.
//
// Like PreviewLabel the fold is not injective — "api_v1" and "api.v1" both
// yield "api-v1" — which is a manifest bug in the same class as two services
// sharing a FORGE_PREVIEW_PORT_<NAME>, not something resolved at request time.
func ServiceLabel(name string) string {
	folded := strings.ToLower(strings.TrimSpace(name))
	folded = strings.ReplaceAll(folded, "_", "-")
	return strings.ReplaceAll(folded, ".", "-")
}

// CheckPreviewLabelCollisions reports the first set of bead ids that fold to the
// same PreviewLabel, mirroring the manifest's FORGE_PREVIEW_PORT_<NAME> check:
// a many-to-one mapping is refused up front rather than left to surprise
// someone at request time. Duplicate entries of one id are not a collision.
//
// Labels and ids are both scanned in sorted order, so the same input always
// produces the same error.
func CheckPreviewLabelCollisions(beadIDs []string) error {
	byLabel := make(map[string]map[string]bool, len(beadIDs))
	for _, id := range beadIDs {
		if strings.TrimSpace(id) == "" {
			continue
		}
		label := PreviewLabel(id)
		if byLabel[label] == nil {
			byLabel[label] = make(map[string]bool, 2)
		}
		byLabel[label][id] = true
	}

	labels := make([]string, 0, len(byLabel))
	for label := range byLabel {
		labels = append(labels, label)
	}
	sort.Strings(labels)

	for _, label := range labels {
		if len(byLabel[label]) < 2 {
			continue
		}
		ids := make([]string, 0, len(byLabel[label]))
		for id := range byLabel[label] {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return &PreviewLabelCollisionError{Label: label, BeadIDs: ids}
	}
	return nil
}

// ParsePreviewHost resolves a request's Host header back to the preview it
// addresses: the inverse of PreviewLabel plus an optional service selector.
//
//	<label>.<base>            → (label, "",      true)
//	<label>--<service>.<base> → (label, service, true)
//
// Both host and base are matched case-insensitively with a port suffix and a
// trailing root dot ignored, so "WI-12.Base.:8080" resolves like "wi-12.base".
//
// ok is false for anything else, and deliberately so for the two cases that
// look closest to a hit: the apex itself (host == base, which is the proxy's
// own name and not a preview) and a host with extra labels in front
// ("a.wi-12.base"), which no preview ever produces. An empty base — the feature
// switched off — never matches anything.
func ParsePreviewHost(host, base string) (string, string, bool) {
	base = NormalizeHostname(base)
	host = NormalizeHostname(stripPort(host))
	if base == "" || host == "" {
		return "", "", false
	}

	suffix := "." + base
	if !strings.HasSuffix(host, suffix) {
		return "", "", false
	}
	prefix := host[:len(host)-len(suffix)]
	// Empty prefix is the apex; a dot in it means extra labels.
	if prefix == "" || strings.Contains(prefix, ".") {
		return "", "", false
	}

	label, service := prefix, ""
	// PreviewLabel never emits "--", so at most one separator can be present.
	// Two of them means this is not a name Kiln handed out.
	if strings.Count(prefix, "--") > 1 {
		return "", "", false
	}
	if i := strings.Index(prefix, "--"); i >= 0 {
		label, service = prefix[:i], prefix[i+2:]
		if !isDNSLabel(service) {
			return "", "", false
		}
	}
	if !isDNSLabel(label) {
		return "", "", false
	}
	return label, service, true
}

// NormalizeHostname lowercases a hostname, trims surrounding whitespace and
// drops the trailing root dot, so a configured base and an incoming Host header
// are compared in the same shape.
func NormalizeHostname(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

// stripPort removes a ":port" suffix from a Host header. A host without one
// makes net.SplitHostPort fail, in which case the value is already what we
// want.
func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(strings.TrimSpace(host)); err == nil {
		return h
	}
	return host
}

// isDNSLabel reports whether s is usable as a single hostname label: 1-63
// characters of [a-z0-9-] that neither open nor close with a hyphen. Input is
// expected to be lowercased already.
func isDNSLabel(s string) bool {
	if s == "" || len(s) > maxDNSLabel {
		return false
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

// quoteAll renders ids as a quoted, comma-separated list for error messages.
func quoteAll(ids []string) string {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = fmt.Sprintf("%q", id)
	}
	return strings.Join(quoted, ", ")
}
