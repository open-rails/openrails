// Package redact scrubs credentials out of text that is about to be logged or
// returned to a client. It is a stdlib-only leaf package so any integration can
// use it without an import cycle.
package redact

import (
	"net/url"
	"regexp"
	"strings"
)

// Placeholder replaces a redacted value.
const Placeholder = "REDACTED"

// secretParamRe matches query-parameter NAMES that carry a credential
// (api-key/apikey/api_key, access-token, token, auth, secret, password, sig).
// Deliberately broad: a false positive only costs log detail, a false negative
// leaks a key.
var secretParamRe = regexp.MustCompile(`(?i)(api[-_]?key|access[-_]?token|auth|password|passwd|secret|token|signature|^sig$|^key$)`)

// IsSecretParam reports whether a query-parameter name carries a credential.
func IsSecretParam(name string) bool { return secretParamRe.MatchString(strings.TrimSpace(name)) }

// secretQueryValueRe matches `?name=value` / `&name=value` pairs whose name is
// credential-bearing, in arbitrary text (error strings, log messages) where the
// URL is not separable and cannot be parsed.
var secretQueryValueRe = regexp.MustCompile(`(?i)([?&][a-z0-9_.\-\[\]]*(?:api[-_]?key|access[-_]?token|auth|password|passwd|secret|token|signature)[a-z0-9_.\-\[\]]*=)[^&\s"'` + "`" + `]*`)

// Secrets scrubs credential-bearing query parameters out of arbitrary text.
// Use it on anything derived from an upstream error before logging it or
// returning it to a caller — third-party clients routinely format the full
// request URL into their error strings.
func Secrets(s string) string {
	return secretQueryValueRe.ReplaceAllString(s, "${1}"+Placeholder)
}

// URL returns raw with every credential-bearing query parameter value replaced
// by the placeholder. An unparseable input falls back to Secrets.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return Secrets(raw)
	}
	q := u.Query()
	changed := false
	for name := range q {
		if IsSecretParam(name) {
			q.Set(name, Placeholder)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// StripSecretQuery splits raw into a credential-free URL and the credential
// query parameters that were removed. Callers hold the credential-free URL
// (safe to log, safe to embed in an error) and re-attach the parameters at the
// transport, on a clone of the outbound request.
func StripSecretQuery(raw string) (string, url.Values) {
	u, err := url.Parse(raw)
	if err != nil {
		return raw, nil
	}
	q := u.Query()
	var secret url.Values
	for name, vals := range q {
		if !IsSecretParam(name) {
			continue
		}
		if secret == nil {
			secret = url.Values{}
		}
		secret[name] = vals
		q.Del(name)
	}
	if secret == nil {
		return raw, nil
	}
	u.RawQuery = q.Encode()
	return u.String(), secret
}
