package merchant

// ReservedHostedSlugs is an advisory DEFAULT reserved-slug list for hosted
// products where merchants self-provision by slug over code paths
// (merchant_source=api, #738): platform routes, brand terms, and common
// infrastructure subdomains a hosted product typically must not let a
// merchant claim (seeded from openrails-saas's own list, #750).
//
// ValidateSlug deliberately does NOT consult this: slug MECHANISM (charset,
// length, shape) is this package's job; slug RESERVATION is host policy. A
// host is free to use this list verbatim, extend it, replace it outright, or
// ignore it — nothing in this package reads it.
var ReservedHostedSlugs = []string{
	"www", "api", "app", "admin", "auth",
	"billing", "platform", "openrails", "saas",
	"docs", "status", "support", "dev", "staging",
	"checkout", "pay", "root", "internal",
}
