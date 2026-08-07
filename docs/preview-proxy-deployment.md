# Deploying the Preview Proxy (wildcard DNS, ingress, TLS)

Kiln previews are normally reached at `host:port`. Setting
`settings.preview_proxy_base` makes Hearth front them by hostname instead, on
its own listener — see [Host-based preview
routing](configuration.md#host-based-preview-routing-preview_proxy_base) for
what the setting does and [Security posture: what a preview URL
exposes](configuration.md#security-posture-what-a-preview-url-exposes) for what
it hands out.

This document covers the other half: what has to exist **outside** Forge before
the setting does anything. Nothing here is created by Forge, and none of it is
optional — a preview hostname that does not resolve, or that terminates TLS
against a certificate not covering it, fails before a single byte reaches the
daemon.

> **Where this belongs.** The Forge Helm chart (`skybert-forge`) lives in its own
> repository, so this file is the canonical copy; paste the [Prerequisites](#prerequisites)
> and [Worked example](#worked-example-values) sections into that chart's README
> and adjust the value key paths to the chart's own `values.yaml` if they differ
> from the conventional `ingress.*` shape used below.

## How the request flows

```
browser
  → https://forge-a1b2.preview.forge.skytest.fhi.no/
  → wildcard DNS  *.preview.forge.skytest.fhi.no  → ingress controller LB
  → Ingress wildcard host rule → forge Service : <hearth port>
  → Hearth (internal/web) PreviewProxyMiddleware
      → auth gate (preview_proxy_auth)
      → preview_resolve → 127.0.0.1:<allocated port> inside the Forge pod
```

Two consequences worth internalising before writing any YAML:

- **No new Service and no new port.** Preview services bind loopback *inside the
  Forge container*, and Hearth forwards to them in-process. The wildcard host
  rule routes to the same Service and the same port the dashboard host already
  uses. Do not try to expose `preview_port_range` — those ports are allocated per
  preview at start time, rotate constantly, and are not reachable from outside
  the pod by design.
- **The proxy only exists when the web GUI does.** Host-based routing lives in
  the Hearth 2.0 web server, so `FORGE_WEB_ENABLED` must be on. With it off, the
  wildcard names resolve to an ingress that routes to nothing serving them.

## Prerequisites

Three things must be in place. The examples use the base
`preview.forge.skytest.fhi.no` with the dashboard at `forge.skytest.fhi.no`.

### 1. Wildcard DNS

```
*.preview.forge.skytest.fhi.no.   CNAME   <ingress controller address>
preview.forge.skytest.fhi.no.     CNAME   <ingress controller address>
```

The wildcard is what makes `forge-a1b2.preview.forge.skytest.fhi.no` and
`forge-a1b2--api.preview.forge.skytest.fhi.no` resolve. Both preview forms are
exactly one label deep (`<label>` and `<label>--<service>`), so a single-level
wildcard covers every preview there will ever be — no per-preview DNS record, and
nothing to clean up when a preview is reaped.

**The apex record is not optional.** A DNS wildcard does not match the name it is
anchored at, and Hearth redirects an unauthenticated browser navigation to
`/login` on the apex of `preview_proxy_base`. Without
`preview.forge.skytest.fhi.no` itself resolving and routed, that redirect is a
dead end. Apex traffic never matches a preview hostname, so it falls through the
proxy middleware to the normal dashboard router — serving the login page there is
exactly what is wanted.

### 2. A wildcard host rule on the existing Ingress

Add rules to the Ingress the chart already renders for the dashboard host, both
pointing at the **same Service and port**:

- `*.preview.forge.skytest.fhi.no` — every preview hostname
- `preview.forge.skytest.fhi.no` — the apex, for the login redirect above

A second Ingress object works too, but a second Service does not: there is only
one listener.

If the ingress controller is `ingress-nginx`, previews also need its streaming
defaults relaxed. A preview serves Vite HMR websockets, SSE and long-poll —
connections that stay open for minutes with no bytes flowing — and the default
60-second read timeout severs exactly those:

```yaml
nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
nginx.ingress.kubernetes.io/proxy-buffering: "off"
nginx.ingress.kubernetes.io/proxy-body-size: "0"
```

(Other controllers have equivalents — Traefik `serversTransport`/timeouts,
HAProxy `timeout-server`. The requirement is the same: long-lived, unbuffered
connections.) Hearth's own side of this is already handled — it flushes every
write and passes websocket upgrades through untouched.

### 3. A wildcard TLS certificate issued by DNS-01

`*.preview.forge.skytest.fhi.no` needs a wildcard certificate, and **ACME cannot
issue a wildcard over HTTP-01** — Let's Encrypt only accepts DNS-01 for a
wildcard identifier. The HTTP-01 issuer that serves the dashboard host therefore
cannot serve this one. Use the existing **`azuredns` DNS-01 issuer**, which can.

Request both names on one certificate:

```
*.preview.forge.skytest.fhi.no
preview.forge.skytest.fhi.no
```

The wildcard does not cover its own apex, and the apex is where the login
redirect lands — so both, or the redirect trips a certificate error.

Two operational notes:

- The Azure DNS service principal cert-manager uses needs write access to the
  zone hosting these records; DNS-01 works by writing a `_acme-challenge` TXT
  record, so read-only or zone-scoped-elsewhere credentials fail at issuance
  with no clue at the ingress.
- A wildcard certificate is published in public Certificate Transparency logs.
  That discloses the existence of the preview zone (not individual previews),
  which is one more reason `preview_proxy_auth` defaults to `session`.

## Worked example values

Conventional Helm ingress shape — adjust key paths to the chart's own
`values.yaml`. The point is the structure: two hosts, one backend, one wildcard
TLS secret from the DNS-01 issuer.

```yaml
ingress:
  enabled: true
  className: nginx
  annotations:
    # DNS-01, not HTTP-01: HTTP-01 cannot issue a wildcard certificate.
    cert-manager.io/cluster-issuer: azuredns
    # Previews carry HMR websockets, SSE and long-poll.
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-buffering: "off"
    nginx.ingress.kubernetes.io/proxy-body-size: "0"
  hosts:
    # The dashboard, unchanged.
    - host: forge.skytest.fhi.no
      paths:
        - path: /
          pathType: Prefix
    # Every preview hostname: <label>.<base> and <label>--<service>.<base>.
    - host: '*.preview.forge.skytest.fhi.no'
      paths:
        - path: /
          pathType: Prefix
    # The base apex — where an unauthenticated preview navigation is sent to log in.
    - host: preview.forge.skytest.fhi.no
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: forge-tls
      hosts:
        - forge.skytest.fhi.no
    - secretName: forge-preview-wildcard-tls
      hosts:
        - '*.preview.forge.skytest.fhi.no'
        - preview.forge.skytest.fhi.no

# Hearth 2.0 must be running — the preview proxy lives in the web server.
env:
  FORGE_WEB_ENABLED: "true"

# Rendered into the daemon's forge.yaml (~/.forge/config.yaml).
forgeConfig:
  settings:
    preview_enabled: true
    preview_proxy_base: preview.forge.skytest.fhi.no
    preview_proxy_auth: session      # 'none' serves previews unauthenticated
    preview_max_concurrent: 2
    preview_idle_timeout: 30m
    preview_bind_host: 127.0.0.1     # loopback only; the proxy reaches it in-process
```

All three ingress hosts route to the same backend Service and port — the chart's
existing one. Nothing here adds a Service, a port or a container.

### Why this layout gets the good auth path

With the dashboard on `forge.skytest.fhi.no` and the base at
`preview.forge.skytest.fhi.no`, the two share the parent
`forge.skytest.fhi.no`, which sits below the registrable domain `fhi.no`. Hearth
therefore widens its session cookie to that shared parent and every preview
request is authorised by the session the operator already has — no token minted,
no extra expiry, and signing out of Hearth revokes preview access with it.

Putting previews under an unrelated domain still works, but drops onto the
token path instead: the preview link then carries a short-lived `_forge_token`
which is exchanged for a per-preview cookie on first contact. Both paths are
described in [Auth gating for proxied
previews](configuration.md#auth-gating-for-proxied-previews-preview_proxy_auth).

## Verifying a deployment

```bash
# 1. DNS: both the wildcard and the apex answer.
dig +short anything.preview.forge.skytest.fhi.no
dig +short preview.forge.skytest.fhi.no

# 2. Certificate: issued, and covering both names.
kubectl get certificate forge-preview-wildcard-tls
kubectl describe certificate forge-preview-wildcard-tls   # DNS-01 challenges must be Valid

# 3. Routing and the auth gate: a name with no live preview, unauthenticated.
curl -sS -o /dev/null -w '%{http_code}\n' https://forge-nope.preview.forge.skytest.fhi.no/
# 401 → ingress, TLS and the gate are all working (the gate answers before the lookup,
#        so "no such preview" is deliberately not distinguishable here)
# 404 → routed and reached Hearth, but preview_proxy_auth is 'none'
# 503/404 from the ingress controller itself → the host rule is missing or points elsewhere

# 4. From the Forge box: what the daemon thinks the links are.
forge preview list
```

`forge preview list` printing `https://<label>.<base>/` confirms the daemon
picked the setting up; a link still shaped `http://<host>:<port>/` means
`preview_proxy_base` is unset, invalid (it is validated at load — a scheme, port,
path or leading dot is rejected), or the config never reached the daemon.

## Troubleshooting

| Symptom | Cause and fix |
|---------|---------------|
| Browser shows a certificate error on a preview host | The wildcard certificate is not issued or does not cover the name. `*.<base>` covers one label only — `a.b.<base>` is not covered, but Kiln never produces such a name; an error on the base apex itself means the apex was left off the certificate. |
| Ingress controller 404 for preview hosts, dashboard fine | The wildcard host rule is missing, or is on an Ingress with a different `className`. |
| `401` from Hearth on every preview request, even signed in | The session cookie is not scoped to the preview host. Check that the dashboard host and `preview_proxy_base` share a real registrable parent (they do not if either is an IP, `localhost`, or the shared suffix is a public suffix). If they cannot share one, that is fine — open previews from the dashboard so the link carries a `_forge_token`. |
| Redirected to a login page that then goes nowhere | Expected: the `next` parameter is carried but not yet consumed. Sign in, then reopen the preview from the dashboard. |
| Preview loads but HMR/SSE dies after ~60s | Ingress read/send timeouts and buffering — see the annotations above. |
| `no live preview for <label>` | Routing works; the preview is gone (idle-reaped, PR merged/closed, or evicted). Start it again from the dashboard. |
| Two beads cannot be previewed at once, one fails at start | A label collision — two bead ids folding to one hostname. See [the label mapping rule](configuration.md#host-based-preview-routing-preview_proxy_base). |
