// Package cardholdername keeps a full cardholder name canonical while
// providing a deterministic, explicitly lossy projection for processors that
// expose only first_name and last_name fields.
package cardholdername

import "strings"

// Canonical prefers the full name and otherwise composes the legacy parts.
// Only outer whitespace is trimmed; Unicode, punctuation, case, order, and
// internal spacing remain the caller's source of truth.
func Canonical(nameOnCard, firstName, lastName string) string {
	if full := strings.TrimSpace(nameOnCard); full != "" {
		return full
	}
	parts := make([]string, 0, 2)
	if first := strings.TrimSpace(firstName); first != "" {
		parts = append(parts, first)
	}
	if last := strings.TrimSpace(lastName); last != "" {
		parts = append(parts, last)
	}
	return strings.Join(parts, " ")
}

// Parts returns the legacy provider fields. An explicit full name wins; its
// first token becomes first_name and the remaining text becomes last_name.
// A mononym remains first-name-only rather than inventing a surname.
func Parts(nameOnCard, firstName, lastName string) (string, string) {
	if strings.TrimSpace(nameOnCard) == "" {
		return strings.TrimSpace(firstName), strings.TrimSpace(lastName)
	}
	parts := strings.Fields(nameOnCard)
	if len(parts) == 0 {
		return "", ""
	}
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], strings.Join(parts[1:], " ")
}
