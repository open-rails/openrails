package billing

import (
	"fmt"
	"regexp"
	"strings"
)

// merchantSlugRe is the legal merchant slug: lowercase alnum and hyphens, no
// leading/trailing hyphen, at most 63 characters. The same slug is the AuthKit
// merchant permission-group instance slug in standalone.
var merchantSlugRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// NormalizeMerchantSlug returns the stored form of a merchant slug: trimmed
// and lowercased.
func NormalizeMerchantSlug(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ValidateMerchantSlug reports whether the normalized form of s is a legal
// merchant slug.
func ValidateMerchantSlug(s string) error {
	if !merchantSlugRe.MatchString(NormalizeMerchantSlug(s)) {
		return fmt.Errorf("invalid merchant slug %q: must use lowercase a-z0-9 and hyphens, no leading/trailing hyphen, <=63 chars", s)
	}
	return nil
}

// ReservedMerchantSlugs is the default reserved-slug list for hosted products
// where merchants self-provision by slug (#738): platform routes, brand terms
// and common infrastructure subdomains. The control plane reserves these plus
// its configured ReservedSlugs; ValidateMerchantSlug does not consult it.
var ReservedMerchantSlugs = []string{
	"www", "api", "app", "admin", "auth",
	"billing", "platform", "openrails", "saas",
	"docs", "status", "support", "dev", "staging",
	"checkout", "pay", "root", "internal",
}
