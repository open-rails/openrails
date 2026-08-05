package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

type customerSpendDelegationsDocument struct {
	CustomerID  string                    `json:"customer_id,omitempty"`
	Delegations []customerSpendDelegation `json:"delegations"`
}

type customerSpendDelegation struct {
	Scope    string `json:"scope"`
	ScopeKey string `json:"scope_key"`
	// CustomerID is request-only: supplying it is rejected (the payer comes from
	// the path scope). Never emitted.
	CustomerID string                      `json:"customer_id,omitempty"`
	Windows    []models.BudgetWindowPolicy `json:"windows"`
}

// UnmarshalJSON keeps the delegation wire shape strict. or#893 deleted the
// role_id alias for scope_key — one representation, {scope:"role",
// scope_key:"<role uuid>"} — and strict decoding is the ONE mechanism that
// enforces it: no sentinel field, and any other retired key fails the same way.
func (d *customerSpendDelegation) UnmarshalJSON(raw []byte) error {
	type declared customerSpendDelegation
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var out declared
	if err := decoder.Decode(&out); err != nil {
		if strings.Contains(err.Error(), `unknown field "role_id"`) {
			return retiredWireKeyError(`role_id was removed (or#893): address a role delegation as {"scope":"role","scope_key":"<role uuid>"}`)
		}
		return err
	}
	*d = customerSpendDelegation(out)
	return nil
}

// retiredWireKeyError is a decode failure whose text is written for the caller:
// the retired key and the shape that replaced it. httprequest surfaces it
// verbatim (ClientSafeBindError) instead of collapsing it to invalid_request.
type retiredWireKeyError string

func (e retiredWireKeyError) Error() string                 { return string(e) }
func (e retiredWireKeyError) ClientSafeBindMessage() string { return string(e) }

var _ httprequest.ClientSafeBindError = retiredWireKeyError("")

// GetCustomerSpendDelegations returns the customer policy for sharing the
// customer's own payable balance. The payer is forced to the resolved
// customer/merchant id; the request never accepts a body customer_id (it is
// taken from the :customer_id path scope). Every customer can delegate spend of
// its balance.
func GetCustomerSpendDelegations(r *httprequest.Request) {
	resolved, ok := requireCustomerTreasuryPrincipal(r)
	if !ok {
		return
	}

	store, payer, ok := customerTreasuryStore(r, resolved)
	if !ok {
		return
	}
	rows, err := store.LoadAll(r.Request.Context(), payer)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "spend delegation lookup failed")
		return
	}
	r.SuccessJSON(customerSpendDelegationsDocument{Delegations: customerSpendDelegationsFromRows(rows)})
}

// PutCustomerSpendDelegations replaces the customer policy document.
func PutCustomerSpendDelegations(r *httprequest.Request) {
	resolved, ok := requireCustomerTreasuryPrincipal(r)
	if !ok {
		return
	}

	var doc customerSpendDelegationsDocument
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

	svc, payer, ok := customerTreasurySpendDelegationService(r, resolved)
	if !ok {
		return
	}
	if err := svc.ReplaceInvokerSpendLimits(r.Request.Context(), payer, next); err != nil {
		customerSpendDelegationWriteError(r, err, "spend delegation replace failed")
		return
	}

	r.SuccessJSON(customerSpendDelegationsDocument{Delegations: customerSpendDelegationsFromInputs(next)})
}

// PutCustomerSpendDelegation atomically upserts one addressed delegation and
// leaves every unrelated payer-owned delegation untouched.
func PutCustomerSpendDelegation(r *httprequest.Request) {
	resolved, ok := requireCustomerTreasuryPrincipal(r)
	if !ok {
		return
	}

	var delegation customerSpendDelegation
	if !r.BindJSON(&delegation) {
		return
	}
	next, err := validateCustomerSpendDelegations([]customerSpendDelegation{delegation})
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	svc, payer, ok := customerTreasurySpendDelegationService(r, resolved)
	if !ok {
		return
	}
	if err := svc.SetInvokerSpendLimits(r.Request.Context(), payer, next[0]); err != nil {
		customerSpendDelegationWriteError(r, err, "spend delegation upsert failed")
		return
	}
	r.SuccessJSON(customerSpendDelegationFromInput(next[0]))
}

// ServicePutCustomerSpendDelegations is the merchant-machine counterpart of
// PutCustomerSpendDelegations. Route authentication pins the merchant; the
// path names a payable customer within that merchant.
func ServicePutCustomerSpendDelegations(r *httprequest.Request) {
	var doc customerSpendDelegationsDocument
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
	r.SuccessJSON(customerSpendDelegationsDocument{Delegations: customerSpendDelegationsFromInputs(next)})
}

// ServicePutCustomerSpendDelegation atomically reasserts one payer grant for a
// merchant-authenticated machine caller without touching sibling grants.
func ServicePutCustomerSpendDelegation(r *httprequest.Request) {
	var delegation customerSpendDelegation
	if !r.BindJSON(&delegation) {
		return
	}
	next, err := validateCustomerSpendDelegations([]customerSpendDelegation{delegation})
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
	r.SuccessJSON(customerSpendDelegationFromInput(next[0]))
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

func requireCustomerTreasuryPrincipal(r *httprequest.Request) (*controlplane.ResolvedDelegated, bool) {
	v, ok := r.Get(middleware.DelegatedContextKey)
	if !ok {
		r.ErrorJSON(http.StatusUnauthorized, "delegated principal required")
		return nil, false
	}
	resolved, ok := v.(*controlplane.ResolvedDelegated)
	if !ok || resolved == nil {
		r.ErrorJSON(http.StatusUnauthorized, "delegated principal required")
		return nil, false
	}
	if !customerIDMatchesResolved(r.Param("customer_id"), resolved) {
		r.ErrorJSON(http.StatusForbidden, "customer_scope_mismatch")
		return nil, false
	}
	return resolved, true
}

func customerIDMatchesResolved(customerID string, resolved *controlplane.ResolvedDelegated) bool {
	return middleware.CustomerIDMatchesDelegated(customerID, resolved)
}

func customerTreasuryStore(r *httprequest.Request, resolved *controlplane.ResolvedDelegated) (*admission.InvokerSpendLimitStore, identity.CustomerID, bool) {
	if r.State == nil || r.State.DB == nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return nil, identity.CustomerID(uuid.Nil), false
	}
	if resolved == nil || resolved.MerchantID.IsZero() {
		r.ErrorJSON(http.StatusUnauthorized, "delegated principal invalid")
		return nil, identity.CustomerID(uuid.Nil), false
	}
	return admission.NewInvokerSpendLimitStore(r.State.DB), identity.CustomerID(resolved.MerchantID.UUID()), true
}

func customerTreasurySpendDelegationService(r *httprequest.Request, resolved *controlplane.ResolvedDelegated) (*billingservice.Service, identity.CustomerID, bool) {
	if resolved == nil || resolved.MerchantID.IsZero() {
		r.ErrorJSON(http.StatusUnauthorized, "delegated principal invalid")
		return nil, identity.CustomerID(uuid.Nil), false
	}
	svc, ok := customerSpendDelegationService(r)
	return svc, identity.CustomerID(resolved.MerchantID.UUID()), ok
}

func validateCustomerSpendDelegations(in []customerSpendDelegation) ([]billingservice.InvokerSpendLimitInput, error) {
	out := make([]billingservice.InvokerSpendLimitInput, 0, len(in))
	for i, row := range in {
		if strings.TrimSpace(row.CustomerID) != "" {
			return nil, fmt.Errorf("delegations[%d].customer_id is not allowed", i)
		}
		windows := make([]billingservice.SpendLimitWindowInput, 0, len(row.Windows))
		for _, window := range row.Windows {
			windows = append(windows, billingservice.SpendLimitWindowInput{
				Key: window.Key, WindowSeconds: window.WindowSeconds, Limit: window.Limit, Currency: window.Currency,
			})
		}
		out = append(out, billingservice.InvokerSpendLimitInput{
			Scope: row.Scope, ScopeKey: row.ScopeKey, Windows: windows,
		})
	}
	return billingservice.ValidateInvokerSpendLimitInputs(out)
}

func customerSpendDelegationsFromRows(rows []admission.InvokerSpendLimit) []customerSpendDelegation {
	out := make([]customerSpendDelegation, 0, len(rows))
	for _, row := range rows {
		out = append(out, customerSpendDelegationFromRow(row))
	}
	sort.Slice(out, func(i, j int) bool {
		return spendDelegationKey(out[i].Scope, out[i].ScopeKey) < spendDelegationKey(out[j].Scope, out[j].ScopeKey)
	})
	return out
}

func customerSpendDelegationFromRow(row admission.InvokerSpendLimit) customerSpendDelegation {
	return customerSpendDelegation{
		Scope: budgets.NormalizeScope(row.Scope), ScopeKey: row.ScopeKey, Windows: row.Windows,
	}
}

func customerSpendDelegationsFromInputs(rows []billingservice.InvokerSpendLimitInput) []customerSpendDelegation {
	out := make([]customerSpendDelegation, 0, len(rows))
	for _, row := range rows {
		out = append(out, customerSpendDelegationFromInput(row))
	}
	return out
}

func customerSpendDelegationFromInput(row billingservice.InvokerSpendLimitInput) customerSpendDelegation {
	windows := make([]models.BudgetWindowPolicy, 0, len(row.Windows))
	for _, window := range row.Windows {
		windows = append(windows, models.BudgetWindowPolicy{
			Key: window.Key, WindowSeconds: window.WindowSeconds, Limit: window.Limit, Currency: window.Currency,
		})
	}
	return customerSpendDelegation{
		Scope: row.Scope, ScopeKey: row.ScopeKey, Windows: windows,
	}
}

func spendDelegationKey(scope, scopeKey string) string {
	return budgets.NormalizeScope(scope) + "\x00" + strings.TrimSpace(scopeKey)
}
