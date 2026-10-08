package credential

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/open-rails/openrails/billing"
)

var (
	ErrResourceTokenInvalid          = errors.New("controlplane: invalid access token")
	ErrResourceTokenIssuerUnknown    = errors.New("controlplane: access token issuer is not trusted")
	ErrResourceTokenMerchantNotBound = errors.New("controlplane: access token issuer is not trusted for the merchant")
	ErrResourceTokenUnavailable      = errors.New("controlplane: access token verification unavailable")
	ErrResourceServerNotConfigured   = errors.New("controlplane: no resource server configured")
)

// ResolvedResourceAccess is a verified RFC 9068 access token from a trusted
// issuer: who acts (a user of that issuer, or its client acting for itself),
// the merchant it acts for and what it may do there.
type ResolvedResourceAccess struct {
	// Machine is a client acting for itself (sub = client_id).
	Machine  bool
	Issuer   string
	Subject  string
	ClientID string
	// MerchantID is one of the merchants the issuer is trusted for: the
	// request's, or the only one.
	MerchantID   billing.MerchantID
	MerchantSlug string
	// Permissions is the token's grant (with its issuer's group roles) within
	// the issuer's ceiling.
	Permissions   []string
	Scopes        []string
	SessionID     string
	Email         string
	EmailVerified bool
	Username      string
}

// HasPermission reports whether the access token grants perm.
func (r *ResolvedResourceAccess) HasPermission(perm string) bool {
	if r == nil {
		return false
	}
	for _, grant := range r.Permissions {
		if permissionMatches(grant, perm) {
			return true
		}
	}
	return false
}

// IntersectPermissions is what both grant sets allow: each grant of a that b
// covers, and each of b that a covers.
func IntersectPermissions(a, b []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	covered := func(p string, by []string) bool {
		for _, g := range by {
			if permissionMatches(strings.TrimSpace(g), p) {
				return true
			}
		}
		return false
	}
	for _, p := range a {
		if p = strings.TrimSpace(p); p != "" && covered(p, b) {
			add(p)
		}
	}
	for _, p := range b {
		if p = strings.TrimSpace(p); p != "" && covered(p, a) {
			add(p)
		}
	}
	return out
}

// LooksLikeResourceToken reports a JWT whose header names RFC 9068's typ. It
// reads the unverified header only to route the token to its verifier.
func LooksLikeResourceToken(token string) bool {
	head, _, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok || strings.Count(token, ".") != 2 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(head)
	if err != nil {
		return false
	}
	var header struct {
		Typ string `json:"typ"`
	}
	if json.Unmarshal(raw, &header) != nil {
		return false
	}
	typ := strings.ToLower(strings.TrimSpace(header.Typ))
	return typ == "at+jwt" || typ == "application/at+jwt"
}

// ChallengeError is a refused access token and the response headers its
// client needs to retry (a DPoP challenge).
type ChallengeError struct {
	Code    string
	Headers map[string]string
	Err     error
}

func (e ChallengeError) Error() string { return e.Code + ": " + e.Err.Error() }
func (e ChallengeError) Unwrap() error { return e.Err }
