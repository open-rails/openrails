package credential

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	auth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
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
	// AuthTime is when the token's user signed in at the issuer; zero when
	// the token does not say.
	AuthTime time.Time
}

// FederatedSignInWindow is how recent a trusted issuer's sign-in must be
// for an operation that moves money or grants access: AuthKit's own window.
const FederatedSignInWindow = 15 * time.Minute

// CheckRecentSignIn is nil when the token's user signed in at the issuer
// within FederatedSignInWindow of now; otherwise a step-up whose metadata
// asks the client to re-authorize with max_age=0. A client acting for itself
// has no sign-in of its own (auth.ErrForbidden).
func (r *ResolvedResourceAccess) CheckRecentSignIn(now time.Time) error {
	switch {
	case r == nil:
		return auth.ErrUnauthenticated
	case r.Machine:
		return auth.ErrForbidden
	case !r.AuthTime.IsZero() && !r.AuthTime.After(now) && now.Sub(r.AuthTime) <= FederatedSignInWindow:
		return nil
	}
	return &auth.Challenge{Err: auth.ErrStepUpRequired, MaxAge: FederatedSignInWindow, Metadata: map[string]any{"issuer": r.Issuer, "max_age": 0}}
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

// ResourceUser is a trusted issuer's verified principal on a signed-in
// user's own routes, and the merchants it may act on.
type ResourceUser struct {
	Machine       bool
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	Username      string
	// Merchants are the issuer's merchants the user may act on; Bound all of
	// the issuer's merchants, and Ceiling its permission ceiling.
	Merchants []billing.UserMerchant
	Bound     []billing.MerchantRef
	Ceiling   []string
}

// EquivalentPermissions reports whether each grant set covers the other.
func EquivalentPermissions(a, b []string) bool {
	return coversAll(a, b) && coversAll(b, a)
}

func coversAll(grants, perms []string) bool {
	for _, p := range perms {
		if !covered(strings.TrimSpace(p), grants) {
			return false
		}
	}
	return true
}

func covered(p string, by []string) bool {
	for _, g := range by {
		if permissionMatches(strings.TrimSpace(g), p) {
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

// ResourceTokenRefusal answers a refused access token, with the challenge
// headers its client retries with.
func ResourceTokenRefusal(err error) billingauth.GateError {
	var challenge ChallengeError
	switch {
	case errors.As(err, &challenge):
		refusal := billingauth.Refusal(billing.CodeAccessTokenInvalid)
		switch challenge.Code {
		case billing.CodeDPoPNonceRequired:
			refusal = billingauth.Refusal(billing.CodeDPoPNonceRequired)
		case billing.CodeSenderProofRequired:
			refusal = billingauth.Refusal(billing.CodeSenderProofRequired)
		case billing.CodeCredentialExpired:
			refusal = billingauth.Refusal(billing.CodeCredentialExpired)
		case billing.CodeInsufficientScope:
			refusal = billingauth.Refusal(billing.CodeInsufficientScope)
		}
		refusal.Headers = challenge.Headers
		return refusal
	case errors.Is(err, ErrResourceTokenIssuerUnknown), errors.Is(err, ErrResourceServerNotConfigured):
		return billingauth.Refusal(billing.CodeAccessTokenIssuerUnknown)
	case errors.Is(err, ErrResourceTokenMerchantNotBound):
		return billingauth.Refusal(billing.CodeAccessTokenMerchantNotBound)
	case errors.Is(err, billing.ErrMerchantUnresolved):
		return billingauth.Refusal(billing.CodeMerchantUnresolved)
	case errors.Is(err, ErrResourceTokenUnavailable):
		return billingauth.Refusal(billing.CodeAuthenticationUnavailable)
	}
	return billingauth.Refusal(billing.CodeAccessTokenInvalid)
}
