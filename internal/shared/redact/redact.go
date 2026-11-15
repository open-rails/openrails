// Package redact scrubs credentials out of text that is about to be logged or
// returned to a client. It is a stdlib-only leaf package so any integration can
// use it without an import cycle.
package redact

import (
	"regexp"
)

// Placeholder replaces a redacted value.
const Placeholder = "REDACTED"

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
