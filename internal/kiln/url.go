package kiln

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// DefaultPreviewScheme is the scheme a host-based preview link uses when the
// caller does not know better. A proxy base is a real DNS name fronted by
// Hearth, and anything reachable under one is expected to terminate TLS; a
// caller that knows otherwise (the web layer, which can read the scheme of the
// request it is answering) passes EntryURLOptions.ProxyScheme instead.
const DefaultPreviewScheme = "https"

// EntryURLOptions describes one preview well enough to address it. Both forms
// are supplied together and EntryURL picks between them, so a caller never
// branches on the proxy base itself — see EntryURL for the rule.
type EntryURLOptions struct {
	// BeadID identifies the preview; it is folded to the host label by
	// PreviewLabel. Required for the host-based form.
	BeadID string
	// Service selects one named service of the preview
	// (`<label>--<service>.<base>`). Empty means the manifest's entry service,
	// which is addressed by the bare `<label>.<base>`.
	Service string

	// ProxyBase is settings.preview_proxy_base — the DNS suffix previews are
	// served under. Empty means host-based routing is off.
	ProxyBase string
	// ProxyScheme overrides DefaultPreviewScheme for the host-based form.
	ProxyScheme string
	// ProxyPort is the port to carry on the preview hostname. Hearth answers
	// preview names on its own listener, so a caller answering a request on a
	// non-default port passes that port here.
	ProxyPort string
	// Token is the access token the preview auth gate exchanges for a cookie,
	// appended to the host-based form. TokenParam names the query parameter:
	// minting the credential and naming it both belong to the layer that owns
	// the gate, so neither is decided here.
	Token      string
	TokenParam string

	// Host is the hostname the port form points at — settings
	// .preview_public_host, or whatever the caller resolved in its place.
	Host string
	// Port is the port the entry service binds. 0 means ports have not been
	// allocated yet, which the port form has no link for.
	Port int
}

// EntryURL builds the link an operator opens for a preview. It is the single
// place either form of a preview address is assembled, so the dashboard, the
// bead panel and `forge preview list` cannot drift apart.
//
// With settings.preview_proxy_base configured the preview is addressed by
// hostname — `<scheme>://<label>[--<service>].<base>[:port]/`, fronted by
// Hearth's own listener — and that form wins outright: where host-based routing
// is on, the loopback port a service binds is frequently not reachable by
// whoever is reading the link, so a proxy base that yields no host (no bead id,
// say) produces no link rather than a port link nobody can open.
//
// Without a proxy base the link is the port the entry service actually binds:
// `http://<host>:<port>/`. The scheme is plain http there because preview
// services bind a bare port and sit behind no TLS of their own.
//
// The result is "" when neither form is available.
func EntryURL(opts EntryURLOptions) string {
	if base := NormalizeHostname(opts.ProxyBase); base != "" {
		return proxyEntryURL(opts, base)
	}
	host := strings.TrimSpace(opts.Host)
	if host == "" || opts.Port <= 0 {
		return ""
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(opts.Port)) + "/"
}

// proxyEntryURL renders the host-based form against an already-normalized base.
func proxyEntryURL(opts EntryURLOptions, base string) string {
	// PreviewLabel folds an empty id to a real label ("preview"), which would
	// be an address for a preview nobody can name — so the id is checked, not
	// the label it produces.
	if strings.TrimSpace(opts.BeadID) == "" {
		return ""
	}
	prefix := PreviewLabel(opts.BeadID)
	if svc := ServiceLabel(opts.Service); svc != "" {
		prefix += "--" + svc
	}
	host := prefix + "." + base
	if port := strings.TrimSpace(opts.ProxyPort); port != "" {
		host = net.JoinHostPort(host, port)
	}
	scheme := strings.ToLower(strings.TrimSpace(opts.ProxyScheme))
	if scheme == "" {
		scheme = DefaultPreviewScheme
	}
	entry := scheme + "://" + host + "/"
	if opts.Token != "" && opts.TokenParam != "" {
		entry += "?" + opts.TokenParam + "=" + url.QueryEscape(opts.Token)
	}
	return entry
}
