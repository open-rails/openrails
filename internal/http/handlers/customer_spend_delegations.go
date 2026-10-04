package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/budgets"
	billingservice "github.com/open-rails/openrails/internal/service"
)

type CustomerSpendDelegationsDocument struct {
	CustomerID  string                    `json:"customer_id,omitempty"`
	Delegations []CustomerSpendDelegation `json:"delegations"`
}

// customerSpendDelegation is the shared Client wire type with strict decoding.
// A per-delegation customer_id is an unknown field: the payer comes from the
// path scope.
type CustomerSpendDelegation billing.SpendDelegationInput

// UnmarshalJSON keeps the delegation wire shape strict. or#893 deleted the
// role_id alias for scope_key — one representation, {scope:"role",
// scope_key:"<role uuid>"} — and strict decoding is the ONE mechanism that
// enforces it: no sentinel field, and any other retired key fails the same way.
func (d *CustomerSpendDelegation) UnmarshalJSON(raw []byte) error {
	type declared CustomerSpendDelegation
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var out declared
	if err := decoder.Decode(&out); err != nil {
		if strings.Contains(err.Error(), `unknown field "role_id"`) {
			return wireKeyError(`role_id was removed (or#893): address a role delegation as {"scope":"role","scope_key":"<role uuid>"}`)
		}
		if strings.Contains(err.Error(), `unknown field "customer_id"`) {
			return wireKeyError("delegations[].customer_id is not allowed; the payer is taken from the path scope")
		}
		return err
	}
	*d = CustomerSpendDelegation(out)
	return nil
}

// wireKeyError is a decode failure whose text is written for the caller:
// the refused key and the shape to use instead. httprequest surfaces it
// verbatim (ClientSafeBindError) instead of collapsing it to invalid_request.
type wireKeyError string

func (e wireKeyError) Error() string                 { return string(e) }
func (e wireKeyError) ClientSafeBindMessage() string { return string(e) }

var _ httprequest.ClientSafeBindError = wireKeyError("")

// ServicePutCustomerSpendDelegations is the merchant-machine counterpart of
// PutCustomerSpendDelegations. Route authentication pins the merchant; the
// path names a payable customer within that merchant.
func ServicePutCustomerSpendDelegations(r *httprequest.Request) {
	var doc CustomerSpendDelegationsDocument
	if !r.BindJSON(&doc) {
		return
	}
	if strings.TrimSpace(doc.CustomerID) != "" {
		r.ErrorJSON(http.StatusBadRequest, "customer_id is not allowed in the body; it is taken from the path scope")
		return
	}
	next, err := validateCustomerSpendDelegations(doc.Delegations)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	svc, payer, ok := serviceCustomerSpendDelegationService(r)
	if !ok {
		return
	}
	if err := svc.ReplaceInvokerSpendLimits(r.Request.Context(), payer, next); err != nil {
		customerSpendDelegationWriteError(r, err, "spend delegation replace failed")
		return
	}
	r.SuccessJSON(CustomerSpendDelegationsDocument{Delegations: customerSpendDelegationsFromInputs(next)})
}

// ServicePutCustomerSpendDelegation atomically reasserts one payer grant for a
// merchant-authenticated machine caller without touching sibling grants.
func ServicePutCustomerSpendDelegation(r *httprequest.Request) {
	var delegation CustomerSpendDelegation
	if !r.BindJSON(&delegation) {
		return
	}
	next, err := validateCustomerSpendDelegations([]CustomerSpendDelegation{delegation})
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	svc, payer, ok := serviceCustomerSpendDelegationService(r)
	if !ok {
		return
	}
	if err := svc.SetInvokerSpendLimits(r.Request.Context(), payer, next[0]); err != nil {
		customerSpendDelegationWriteError(r, err, "spend delegation upsert failed")
		return
	}
	r.SuccessJSON(CustomerSpendDelegation(next[0]))
}

// ServiceDeleteCustomerSpendDelegation is the merchant-machine counterpart of
// DeleteCustomerSpendDelegation.
func ServiceDeleteCustomerSpendDelegation(r *httprequest.Request) {
	svc, payer, ok := serviceCustomerSpendDelegationService(r)
	if !ok {
		return
	}
	deleteCustomerSpendDelegation(r, svc, payer)
}

func deleteCustomerSpendDelegation(r *httprequest.Request, svc *billingservice.Service, payer identity.CustomerID) {
	scope := r.Param("scope")
	scopeKey := r.Param("scope_key")
	deleted, err := svc.DeleteInvokerSpendLimit(r.Request.Context(), payer, scope, scopeKey)
	if err != nil {
		customerSpendDelegationWriteError(r, err, "spend delegation delete failed")
		return
	}
	if !deleted {
		r.ErrorCode("spend_delegation_not_found", "")
		return
	}
	r.SuccessJSON(map[string]any{"deleted": true})
}

func serviceCustomerSpendDelegationService(r *httprequest.Request) (*billingservice.Service, identity.CustomerID, bool) {
	payer, err := parseServiceCustomerID(r.Param("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return nil, identity.CustomerID(uuid.Nil), false
	}
	if !requireServiceCustomerScope(r, *payer) {
		return nil, identity.CustomerID(uuid.Nil), false
	}
	svc, ok := customerSpendDelegationService(r)
	return svc, *payer, ok
}

func customerSpendDelegationService(r *httprequest.Request) (*billingservice.Service, bool) {
	if r.State == nil || r.State.DB == nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return nil, false
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return nil, false
	}
	return svc, true
}

func customerSpendDelegationWriteError(r *httprequest.Request, err error, internalMessage string) {
	if errors.Is(err, billingservice.ErrInvalidInvokerSpendLimit) {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.ErrorJSON(http.StatusInternalServerError, internalMessage)
}

func validateCustomerSpendDelegations(in []CustomerSpendDelegation) ([]billingservice.InvokerSpendLimitInput, error) {
	out := make([]billingservice.InvokerSpendLimitInput, 0, len(in))
	for _, row := range in {
		out = append(out, billingservice.InvokerSpendLimitInput(row))
	}
	return billingservice.ValidateInvokerSpendLimitInputs(out)
}

func customerSpendDelegationsFromRows(rows []admission.InvokerSpendLimit) []CustomerSpendDelegation {
	out := make([]CustomerSpendDelegation, 0, len(rows))
	for _, row := range rows {
		out = append(out, customerSpendDelegationFromRow(row))
	}
	sort.Slice(out, func(i, j int) bool {
		return spendDelegationKey(out[i].Scope, out[i].ScopeKey) < spendDelegationKey(out[j].Scope, out[j].ScopeKey)
	})
	return out
}

func customerSpendDelegationFromRow(row admission.InvokerSpendLimit) CustomerSpendDelegation {
	windows := make([]billing.SpendLimitWindow, 0, len(row.Windows))
	for _, window := range row.Windows {
		windows = append(windows, billing.SpendLimitWindow{
			Key: window.Key, WindowSeconds: window.WindowSeconds, Limit: window.Limit, Currency: window.Currency,
		})
	}
	return CustomerSpendDelegation{
		Scope: budgets.NormalizeScope(row.Scope), ScopeKey: row.ScopeKey, Windows: windows,
		Provenance: row.Provenance,
	}
}

func customerSpendDelegationsFromInputs(rows []billingservice.InvokerSpendLimitInput) []CustomerSpendDelegation {
	out := make([]CustomerSpendDelegation, 0, len(rows))
	for _, row := range rows {
		out = append(out, CustomerSpendDelegation(row))
	}
	return out
}

func spendDelegationKey(scope, scopeKey string) string {
	return budgets.NormalizeScope(scope) + "\x00" + strings.TrimSpace(scopeKey)
}
