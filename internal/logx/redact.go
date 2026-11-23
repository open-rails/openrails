package logx

import (
	"regexp"
	"strings"

	"github.com/open-rails/openrails/internal/shared/redact"
)

var (
	// A card number: 13 to 19 digits, optionally grouped by spaces or dashes.
	panPattern = regexp.MustCompile(`\b\d(?:[ -]?\d){12,18}\b`)
	// A credential named by its key, in a query string, header, JSON or
	// key=value text.
	namedSecret = regexp.MustCompile(`(?i)("?\b(?:authorization|api[_-]?key|security[_-]?key|secret[_-]?key|client[_-]?secret|secret|password|passwd|private[_-]?key|card[_-]?number|ccnumber|cvv2?|cvc|access[_-]?token|refresh[_-]?token|token)\b"?\s*[:=]\s*"?)[^"&,\s}]+`)
	// A credential recognizable by its own shape.
	shapedSecret = regexp.MustCompile(`\b(?:(?:sk|rk)_(?:live|test)_[0-9A-Za-z_]+|whsec_[0-9A-Za-z_]+|hvs\.[0-9A-Za-z_-]+)|(?i:\b(?:bearer|basic)\s+[0-9A-Za-z._~+/=-]+)|://[^/\s:@]+:[^/\s@]+@`)
)

// Redact is s without card numbers or credentials, credential-bearing query
// parameters (redact.Secrets) included.
func Redact(s string) string {
	s = panPattern.ReplaceAllStringFunc(s, func(m string) string {
		if luhn(m) {
			return "[card]"
		}
		return m
	})
	s = shapedSecret.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(m, "://") {
			return "://[redacted]@"
		}
		return "[redacted]"
	})
	return namedSecret.ReplaceAllString(redact.Secrets(s), "${1}[redacted]")
}

func luhn(s string) bool {
	sum, n := 0, 0
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c < '0' || c > '9' {
			continue
		}
		d := int(c - '0')
		if n%2 == 1 {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		n++
	}
	return n >= 13 && sum%10 == 0
}
