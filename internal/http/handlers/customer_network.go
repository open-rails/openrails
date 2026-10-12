package handlers

import (
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/customernetwork"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/paymentpage"
)

// networkProof is the merchant side the customer's request proves: the
// customer at its merchant, by its own token or, on the payment origin, by
// its order's pay URL.
func networkProof(r *httprequest.Request) (customernetwork.Network, customernetwork.Proof, bool) {
	scope, ok := r.CustomerScope()
	if !ok {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return nil, customernetwork.Proof{}, false
	}
	if r.State == nil || r.State.Network == nil {
		r.ErrorCode(billing.CodeRouteNotFound, "")
		return nil, customernetwork.Proof{}, false
	}
	proof := customernetwork.Proof{Merchant: scope.Merchant(), Customer: scope.Customer(), Credential: customernetwork.CustomerToken}
	if c, ok := paymentpage.FromContext(r.Request.Context()); ok {
		proof.Credential, proof.Order = customernetwork.PayURL, c.OrderID
	}
	return r.State.Network, proof, true
}

// CreateNetworkLinkChallenge handles POST /v1/me/network-link/challenges.
func CreateNetworkLinkChallenge(r *httprequest.Request) {
	network, proof, ok := networkProof(r)
	if !ok {
		return
	}
	challenge, err := network.Challenge(r.Request.Context(), proof)
	if err != nil {
		writeRefusal(r, err, "network link challenge failed")
		return
	}
	r.JSON(http.StatusCreated, challenge)
}

// CreateNetworkLink handles POST /v1/me/network-link: 201 with the new link,
// 200 when the customer was already linked to that account.
func CreateNetworkLink(r *httprequest.Request) {
	var params billing.CreateNetworkLinkParams
	if !r.BindJSON(&params) {
		return
	}
	network, proof, ok := networkProof(r)
	if !ok {
		return
	}
	link, created, err := network.Link(r.Request.Context(), proof, params)
	if err != nil {
		writeRefusal(r, err, "network link failed")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	r.JSON(status, link)
}

// ListNetworkCustomers handles GET /v1/app/customers: the merchant's
// customer linked to an openrails.dev account, in a page of at most one.
func ListNetworkCustomers(r *httprequest.Request) {
	raw := strings.TrimSpace(r.Query("network_account_id"))
	account, err := billing.ParseNetworkAccountID(raw)
	if raw == "" || err != nil || account.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidQuery, "network_account_id is required: an openrails.dev account id").WithParam("network_account_id"))
		return
	}
	if r.State == nil || r.State.Network == nil {
		r.ErrorCode(billing.CodeRouteNotFound, "")
		return
	}
	ctx := r.Request.Context()
	mid, err := merchant.Require(ctx)
	if err != nil {
		writeRefusal(r, err, "network customer lookup failed")
		return
	}
	page := billing.ListPage[billing.Customer]{Items: []billing.Customer{}}
	id, found, err := r.State.Network.CustomerOf(ctx, mid, account)
	if err != nil {
		writeRefusal(r, err, "network customer lookup failed")
		return
	}
	if found {
		svc, ok := billingService(r)
		if !ok {
			return
		}
		if page, err = svc.ListCustomers(ctx, billing.CustomerListParams{IDs: []billing.CustomerID{id}}); err != nil {
			writeRefusal(r, err, "network customer lookup failed")
			return
		}
	}
	r.SuccessJSON(page)
}
