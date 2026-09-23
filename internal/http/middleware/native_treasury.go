package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/billingauth"
)

const nativeTreasuryKey = "openrails.native_treasury"

// NativeTreasuryAuthority is installed only after constructor-native session
// verification. Personal ownership and a selected merchant's payer account are
// distinct; non-personal operations require a live, exact-target decision.
type NativeTreasuryAuthority struct {
	CustomerID string
	Target     billingauth.Target
	Authorize  func(context.Context, string, billingauth.Target) error
}

func SetNativeTreasuryAuthority(r *request.Request, authority NativeTreasuryAuthority) {
	r.Set(nativeTreasuryKey, authority)
}

func nativeTreasuryFromRequest(r *request.Request) (NativeTreasuryAuthority, bool) {
	v, ok := r.Get(nativeTreasuryKey)
	if !ok {
		return NativeTreasuryAuthority{}, false
	}
	authority, ok := v.(NativeTreasuryAuthority)
	return authority, ok
}

func bindNativeTreasuryPayer(r *request.Request, authority NativeTreasuryAuthority) (*TreasuryPayer, bool) {
	selected := strings.TrimSpace(r.Param("customer_id"))
	payer := &TreasuryPayer{Subject: authority.CustomerID}
	switch {
	case selected == authority.CustomerID && authority.CustomerID != "":
		payer.CustomerID = identity.CustomerIDFromString(authority.CustomerID)
	case selected == authority.Target.MerchantID.String() || selected == authority.Target.MerchantSlug:
		payer.CustomerID = identity.CustomerID(authority.Target.MerchantID.UUID())
		payer.MerchantPayer = true
	default:
		return nil, false
	}
	if payer.CustomerID.IsZero() {
		return nil, false
	}
	authority.Target.CustomerID = payer.CustomerID.String()
	SetNativeTreasuryAuthority(r, authority)
	return payer, true
}

func requireNativeTreasuryPermission(r *request.Request, authority NativeTreasuryAuthority, permission string) bool {
	if authority.Target.CustomerID != "" && authority.Target.CustomerID == authority.CustomerID {
		return true
	}
	if authority.Authorize == nil {
		r.AbortJSON(http.StatusForbidden, "customer_scope_mismatch")
		return false
	}
	if err := authority.Authorize(r.Request.Context(), permission, authority.Target); err != nil {
		var gate billingauth.GateError
		if errors.As(err, &gate) && gate.Status >= 400 && gate.Status <= 599 {
			r.AbortJSON(gate.Status, gate.Message)
		} else {
			r.AbortJSON(http.StatusServiceUnavailable, "authorization unavailable")
		}
		return false
	}
	return true
}
