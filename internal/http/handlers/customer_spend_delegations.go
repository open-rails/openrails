package handlers

import (
	"context"
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
)

type customerSpendDelegationsDocument struct {
	CustomerID  string                    `json:"customer_id,omitempty"`
	Delegations []customerSpendDelegation `json:"delegations"`
}

type customerSpendDelegation struct {
	Scope      string                      `json:"scope"`
	ScopeKey   string                      `json:"scope_key"`
	RoleID     string                      `json:"role_id,omitempty"`
	CustomerID string                      `json:"customer_id,omitempty"`
	Windows    []models.BudgetWindowPolicy `json:"windows"`
}

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

	store, payer, ok := customerTreasuryStore(r, resolved)
	if !ok {
		return
	}
	if err := replaceCustomerSpendDelegations(r.Request.Context(), store, payer, next); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "spend delegation replace failed")
		return
	}

	r.SuccessJSON(customerSpendDelegationsDocument{Delegations: customerSpendDelegationsFromRows(next)})
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

	store, payer, ok := customerTreasuryStore(r, resolved)
	if !ok {
		return
	}
	if err := store.Upsert(r.Request.Context(), payer, next[0]); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "spend delegation upsert failed")
		return
	}
	r.SuccessJSON(customerSpendDelegationFromRow(next[0]))
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
	store, payer, ok := serviceCustomerSpendDelegationStore(r)
	if !ok {
		return
	}
	if err := replaceCustomerSpendDelegations(r.Request.Context(), store, payer, next); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "spend delegation replace failed")
		return
	}
	r.SuccessJSON(customerSpendDelegationsDocument{Delegations: customerSpendDelegationsFromRows(next)})
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
	store, payer, ok := serviceCustomerSpendDelegationStore(r)
	if !ok {
		return
	}
	if err := store.Upsert(r.Request.Context(), payer, next[0]); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "spend delegation upsert failed")
		return
	}
	r.SuccessJSON(customerSpendDelegationFromRow(next[0]))
}

func serviceCustomerSpendDelegationStore(r *httprequest.Request) (*admission.InvokerSpendLimitStore, identity.CustomerID, bool) {
	payer, err := parseServiceCustomerID(r.Param("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return nil, identity.CustomerID(uuid.Nil), false
	}
	if !requireServiceCustomerScope(r, *payer) {
		return nil, identity.CustomerID(uuid.Nil), false
	}
	if r.State == nil || r.State.DB == nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return nil, identity.CustomerID(uuid.Nil), false
	}
	return admission.NewInvokerSpendLimitStore(r.State.DB), *payer, true
}

func replaceCustomerSpendDelegations(ctx context.Context, store *admission.InvokerSpendLimitStore, payer identity.CustomerID, next []admission.InvokerSpendLimit) error {
	existing, err := store.LoadAll(ctx, payer)
	if err != nil {
		return err
	}
	wanted := make(map[string]admission.InvokerSpendLimit, len(next))
	for _, row := range next {
		wanted[spendDelegationKey(row.Scope, row.ScopeKey)] = row
	}
	for _, row := range existing {
		if _, keep := wanted[spendDelegationKey(row.Scope, row.ScopeKey)]; keep {
			continue
		}
		if err := store.Delete(ctx, payer, row.Scope, row.ScopeKey); err != nil {
			return err
		}
	}
	for _, row := range next {
		if err := store.Upsert(ctx, payer, row); err != nil {
			return err
		}
	}
	return nil
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

func validateCustomerSpendDelegations(in []customerSpendDelegation) ([]admission.InvokerSpendLimit, error) {
	out := make([]admission.InvokerSpendLimit, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, row := range in {
		if strings.TrimSpace(row.CustomerID) != "" {
			return nil, fmt.Errorf("delegations[%d].customer_id is not allowed", i)
		}
		scope := budgets.NormalizeScope(row.Scope)
		scopeKey := strings.TrimSpace(row.ScopeKey)
		roleID := strings.TrimSpace(row.RoleID)
		if roleID != "" && scope != budgets.ScopeRole {
			return nil, fmt.Errorf("delegations[%d].role_id is only allowed for scope %q", i, budgets.ScopeRole)
		}
		if scopeKey != "" && roleID != "" && scopeKey != roleID {
			return nil, fmt.Errorf("delegations[%d].role_id must match scope_key", i)
		}
		if scopeKey == "" {
			scopeKey = roleID
		}
		windows := append([]models.BudgetWindowPolicy(nil), row.Windows...)
		normalized, err := admission.ValidateInvokerSpendLimit(admission.InvokerSpendLimit{
			Scope: scope, ScopeKey: scopeKey, Windows: windows,
		})
		if err != nil {
			return nil, fmt.Errorf("delegations[%d].%s", i, err)
		}
		key := spendDelegationKey(normalized.Scope, normalized.ScopeKey)
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("duplicate delegation for %s", key)
		}
		seen[key] = struct{}{}
		out = append(out, normalized)
	}
	sort.Slice(out, func(i, j int) bool {
		return spendDelegationKey(out[i].Scope, out[i].ScopeKey) < spendDelegationKey(out[j].Scope, out[j].ScopeKey)
	})
	return out, nil
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
	delegation := customerSpendDelegation{
		Scope: budgets.NormalizeScope(row.Scope), ScopeKey: row.ScopeKey, Windows: row.Windows,
	}
	if delegation.Scope == budgets.ScopeRole {
		delegation.RoleID = row.ScopeKey
	}
	return delegation
}

func spendDelegationKey(scope, scopeKey string) string {
	return budgets.NormalizeScope(scope) + "\x00" + strings.TrimSpace(scopeKey)
}
