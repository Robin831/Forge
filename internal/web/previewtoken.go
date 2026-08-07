package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Preview access tokens — the fallback half of the preview auth gate.
//
// When sharedCookieDomain says the Hearth session cookie cannot legitimately
// be widened to cover preview hostnames (different registrable domains, an IP
// address, a public suffix in between), the browser arrives at a preview host
// carrying nothing. A token bridges that gap: Hearth mints one into the
// preview link it renders for an already-authenticated operator, the proxy
// verifies it on the first request, and immediately exchanges it for a
// preview-scoped cookie so it never appears in a URL again.
//
// The token is a bearer credential in a query string, which is why it says as
// little as possible and expires almost at once: it names one preview label
// and one deadline, and it is not a Hearth session — it grants exactly "this
// preview, for a while", never an API call.

const (
	// previewTokenTTL bounds a token handed out in a link. Long enough that an
	// operator can read the dashboard before clicking, short enough that a
	// link pasted into a chat window is dead by the time anyone else opens it.
	previewTokenTTL = 2 * time.Minute

	// previewCookieTTL bounds the cookie the token is exchanged for. This is
	// the credential doing the real work — a review session on a preview runs
	// for hours — so it is longer, but still well under the Hearth session's
	// own lifetime and still scoped to the single preview label it was minted
	// for.
	previewCookieTTL = 8 * time.Hour

	// previewTokenParam is the query parameter a minted token rides in.
	previewTokenParam = "_forge_token"

	// previewTokenCookieName is the host-only cookie the token is exchanged
	// for. Set without a Domain, so the browser scopes it to the exact preview
	// hostname that issued it.
	previewTokenCookieName = "forge_preview"
)

// Distinct failures, because they mean different things to whoever is looking
// at the 401: a malformed or tampered token is somebody probing, an expired one
// is an operator who left the tab open, a mismatched label is a link for a
// different preview.
var (
	errTokenMalformed     = errors.New("preview token is malformed or not signed by this daemon")
	errTokenExpired       = errors.New("preview token has expired")
	errTokenLabelMismatch = errors.New("preview token was issued for a different preview")
)

// mintPreviewToken returns a token authorising access to the preview named by
// label until exp.
//
// The wire form is base64url(payload) "." base64url(HMAC-SHA256(payload)),
// where payload is "<label>|<unix expiry>". Both halves are raw-URL encoded so
// the result is safe in a query string without further escaping.
func mintPreviewToken(secret []byte, label string, exp time.Time) string {
	payload := label + "|" + strconv.FormatInt(exp.Unix(), 10)
	return encodeToken([]byte(payload), signPreviewPayload(secret, payload))
}

// verifyPreviewToken checks that token was signed by this daemon, has not
// expired, and names the given label. It returns nil only when all three hold.
//
// The signature is checked first and with hmac.Equal: an attacker must not be
// able to learn anything from how long the comparison took, nor to get an
// expiry or label answer out of a payload nobody signed.
func verifyPreviewToken(secret []byte, token, label string, now time.Time) error {
	rawPayload, mac, ok := decodeToken(token)
	if !ok {
		return errTokenMalformed
	}
	if !hmac.Equal(mac, signPreviewPayload(secret, string(rawPayload))) {
		return errTokenMalformed
	}
	gotLabel, expUnix, ok := splitPreviewPayload(string(rawPayload))
	if !ok {
		return errTokenMalformed
	}
	if now.After(time.Unix(expUnix, 0)) {
		return errTokenExpired
	}
	if gotLabel != label {
		return errTokenLabelMismatch
	}
	return nil
}

// signPreviewPayload is the one place the MAC is computed, so minting and
// verification cannot drift apart.
func signPreviewPayload(secret []byte, payload string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(payload))
	return m.Sum(nil)
}

func encodeToken(payload, mac []byte) string {
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(mac)
}

// decodeToken splits and decodes the two halves of the wire form. Anything that
// is not exactly "<b64>.<b64>" is rejected here rather than producing a partial
// payload for the caller to reason about.
func decodeToken(token string) (payload, mac []byte, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, nil, false
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(parts[0])
	if err != nil {
		return nil, nil, false
	}
	mac, err = enc.DecodeString(parts[1])
	if err != nil || len(mac) != sha256.Size {
		return nil, nil, false
	}
	return payload, mac, true
}

// splitPreviewPayload parses "<label>|<unix expiry>". The label is matched
// literally against the host's label by the caller, so it is not normalised
// here — a token whose label does not compare equal is simply not for this
// preview.
func splitPreviewPayload(payload string) (label string, expUnix int64, ok bool) {
	i := strings.LastIndex(payload, "|")
	if i <= 0 {
		return "", 0, false
	}
	exp, err := strconv.ParseInt(payload[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return payload[:i], exp, true
}
