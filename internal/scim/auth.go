package scim

import (
	"errors"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/credential"
)

// Authenticator resolves the merchant a SCIM request acts for: its
// provisioning token's, or, with Resource, a client-credentials access
// token's. Bound, when it answers a merchant, is the only one admitted: an
// embedded mount serves its merchant alone.
type Authenticator struct {
	Tokens   Tokens
	Bound    func() billing.MerchantID
	Resource func(*http.Request) (billing.MerchantID, error)
}

// Authenticate is Server.Authenticate.
func (a Authenticator) Authenticate(r *http.Request) (billing.MerchantID, error) {
	token := ""
	if fields := strings.Fields(r.Header.Get("Authorization")); len(fields) == 2 && (strings.EqualFold(fields[0], "Bearer") || strings.EqualFold(fields[0], "DPoP")) {
		token = fields[1]
	}
	var mid billing.MerchantID
	var err error
	if credential.LooksLikeResourceToken(token) {
		if a.Resource == nil {
			return mid, ErrUnauthorized
		}
		if mid, err = a.Resource(r); err != nil {
			return mid, resourceRefusal(err)
		}
	} else if mid, err = a.Tokens.Resolve(r.Context(), token); err != nil {
		return mid, err
	}
	if a.Bound != nil {
		if bound := a.Bound(); !bound.IsZero() && bound != mid {
			return billing.MerchantID{}, ErrUnauthorized
		}
	}
	return mid, nil
}

// resourceRefusal classifies a refused access token: one for another
// merchant or without scope scim is forbidden, any other unauthorized.
func resourceRefusal(err error) error {
	var challenge credential.ChallengeError
	switch {
	case errors.Is(err, credential.ErrResourceTokenMerchantNotBound):
		return ErrForbidden
	case errors.As(err, &challenge) && challenge.Code == billing.CodeInsufficientScope:
		return ErrForbidden
	case errors.Is(err, credential.ErrResourceTokenUnavailable):
		return err
	}
	return ErrUnauthorized
}
