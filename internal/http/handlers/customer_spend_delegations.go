package handlers

import (
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

// ListSpendDelegations lists the delegations that let invokers spend a
// customer's balance.
func ListSpendDelegations(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	delegations, err := svc.InvokerSpendLimits(r.Request.Context(), customer)
	if err != nil {
		writeMoneyError(r, err, "spend delegation list failed")
		return
	}
	r.SuccessJSON(billing.ListPage[billing.SpendDelegation]{Items: delegations})
}

// SetSpendDelegations replaces a customer's spend delegations.
func SetSpendDelegations(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	var params billing.SetSpendDelegationsParams
	if !r.BindJSON(&params) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	if err := svc.ReplaceInvokerSpendLimits(r.Request.Context(), customer, params.Delegations); err != nil {
		writeMoneyError(r, err, "spend delegation replace failed")
		return
	}
	delegations, err := svc.InvokerSpendLimits(r.Request.Context(), customer)
	if err != nil {
		writeMoneyError(r, err, "spend delegation list failed")
		return
	}
	r.SuccessJSON(billing.ListPage[billing.SpendDelegation]{Items: delegations})
}

func spendDelegationAddress(r *httprequest.Request) (billing.SpendDelegationScope, string) {
	return billing.SpendDelegationScope(r.Param("scope")), r.Param("scope_key")
}

// SetSpendDelegation sets the delegation at its scope and key, leaving the
// customer's other delegations untouched.
func SetSpendDelegation(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	var params billing.SetSpendDelegationParams
	if !r.BindJSON(&params) {
		return
	}
	scope, key := spendDelegationAddress(r)
	svc, ok := billingService(r)
	if !ok {
		return
	}
	delegation := billing.SpendDelegation{Scope: scope, ScopeKey: key, Windows: params.Windows, Provenance: normalize.OptionalString(params.Provenance)}
	set, err := svc.SetInvokerSpendLimit(r.Request.Context(), customer, delegation)
	if err != nil {
		writeMoneyError(r, err, "spend delegation update failed")
		return
	}
	r.SuccessJSON(set)
}

// DeleteSpendDelegation revokes the delegation at its scope and key.
func DeleteSpendDelegation(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	scope, key := spendDelegationAddress(r)
	svc, ok := billingService(r)
	if !ok {
		return
	}
	deleted, err := svc.DeleteInvokerSpendLimit(r.Request.Context(), customer, string(scope), key)
	if err != nil {
		writeMoneyError(r, err, "spend delegation delete failed")
		return
	}
	if !deleted {
		r.APIError(api.Coded("spend_delegation_not_found", ""))
		return
	}
	r.Status(http.StatusNoContent)
}
