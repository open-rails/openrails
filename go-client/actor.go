package openrails

import "strings"

// ActorInputs is the narrow projection of an authenticated principal needed to
// derive the canonical OpenRails actor. It mirrors the relevant fields of
// types.AuthInfo without importing it (keeps this package free of HTTP-layer
// deps and trivially testable). Callers populate it via ActorForAuth in the
// HTTP layer.
type ActorInputs struct {
	// Source is the auth path: service_token | platform_delegated_jwt
	// | authkit | api_key | dev.
	Source string
	// ServiceTokenKeyID is the <key_id> parsed from a cozy_st_<key_id>_<secret> token.
	ServiceTokenKeyID string
	// DelegatedIssuerID / DelegatedSub identify a delegated (platform) principal.
	DelegatedIssuerID string
	DelegatedSub      string
	// UserID is the resolved end-user id for a direct (non-delegated) JWT.
	UserID string
	// Subject is the raw JWT subject, used as a UserID fallback.
	Subject string
	// Fallback is the last-resort attribution string (typically the tenant slug)
	// when no finer-grained identity is available.
	Fallback string
}

// ActorFor derives the canonical OpenRails actor (#246):
//
//	service-token:<key_id> for a service-token caller
//	<issuer>:<sub>     for a delegated platform JWT
//	user:<id>          for a direct JWT / user principal
//	<fallback>         when nothing finer is available (e.g. tenant slug)
//
// The granularity matters: OpenRails attributes spend to the actual actor, so
// a service-token key, a delegated platform user, and a direct user are distinct actors
// even under the same payer tenant.
func ActorFor(in ActorInputs) string {
	switch strings.TrimSpace(in.Source) {
	case "service_token":
		if k := strings.TrimSpace(in.ServiceTokenKeyID); k != "" {
			return "service-token:" + k
		}
	case "platform_delegated_jwt":
		iss := strings.TrimSpace(in.DelegatedIssuerID)
		sub := strings.TrimSpace(in.DelegatedSub)
		if iss != "" && sub != "" {
			return iss + ":" + sub
		}
	}
	// Direct user identity (authkit/api_key/dev or any path with a resolved user).
	if u := strings.TrimSpace(in.UserID); u != "" {
		return "user:" + u
	}
	if s := strings.TrimSpace(in.Subject); s != "" {
		return "user:" + s
	}
	return strings.TrimSpace(in.Fallback)
}

// ServiceTokenKeyIDFromToken parses <key_id> out of a cozy_st_<key_id>_<secret> token.
// Returns "" for any non-service-token or malformed token.
func ServiceTokenKeyIDFromToken(token string) string {
	const marker = "cozy_st_"
	t := strings.TrimSpace(token)
	if !strings.HasPrefix(t, marker) {
		return ""
	}
	rest := t[len(marker):]
	// rest is <key_id>_<secret>; the key id is everything up to the first '_'.
	if i := strings.IndexByte(rest, '_'); i > 0 {
		return rest[:i]
	}
	return ""
}
