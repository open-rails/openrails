package handlers

import (
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
	existing, err := store.LoadAll(r.Request.Context(), payer)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "spend delegation lookup failed")
		return
	}

	wanted := make(map[string]admission.InvokerSpendLimit, len(next))
	for _, row := range next {
		wanted[spendDelegationKey(row.Scope, row.ScopeKey)] = row
	}
	for _, row := range existing {
		if _, keep := wanted[spendDelegationKey(row.Scope, row.ScopeKey)]; keep {
			continue
		}
		if err := store.Delete(r.Request.Context(), payer, row.Scope, row.ScopeKey); err != nil {
			r.ErrorJSON(http.StatusInternalServerError, "spend delegation replace failed")
			return
		}
	}
	for _, row := range next {
		if err := store.Upsert(r.Request.Context(), payer, row); err != nil {
			r.ErrorJSON(http.StatusInternalServerError, "spend delegation replace failed")
			return
		}
	}

	r.SuccessJSON(customerSpendDelegationsDocument{Delegations: customerSpendDelegationsFromRows(next)})
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
		if scope != budgets.ScopeInvoker && scope != budgets.ScopeRole && scope != budgets.ScopeInvokerTrustLevel {
			return nil, fmt.Errorf("delegations[%d].scope must be %q, %q, or %q", i, budgets.ScopeInvoker, budgets.ScopeRole, budgets.ScopeInvokerTrustLevel)
		}
		scopeKey := strings.TrimSpace(row.ScopeKey)
		if scopeKey == "" {
			scopeKey = strings.TrimSpace(row.RoleID)
		}
		if scopeKey == "" {
			return nil, fmt.Errorf("delegations[%d].scope_key required", i)
		}
		key := spendDelegationKey(scope, scopeKey)
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("duplicate delegation for %s", key)
		}
		seen[key] = struct{}{}
		if len(row.Windows) == 0 {
			return nil, fmt.Errorf("delegations[%d].windows required", i)
		}
		windows := make([]models.BudgetWindowPolicy, 0, len(row.Windows))
		for j, window := range row.Windows {
			window.Key = strings.TrimSpace(window.Key)
			window.Currency = strings.TrimSpace(window.Currency)
			if window.Key == "" {
				return nil, fmt.Errorf("delegations[%d].windows[%d].key required", i, j)
			}
			if window.WindowSeconds <= 0 {
				return nil, fmt.Errorf("delegations[%d].windows[%d].window_seconds must be positive", i, j)
			}
			if window.Limit < 0 {
				return nil, fmt.Errorf("delegations[%d].windows[%d].limit must be non-negative", i, j)
			}
			windows = append(windows, window)
		}
		out = append(out, admission.InvokerSpendLimit{Scope: scope, ScopeKey: scopeKey, Windows: windows})
	}
	sort.Slice(out, func(i, j int) bool {
		return spendDelegationKey(out[i].Scope, out[i].ScopeKey) < spendDelegationKey(out[j].Scope, out[j].ScopeKey)
	})
	return out, nil
}

func customerSpendDelegationsFromRows(rows []admission.InvokerSpendLimit) []customerSpendDelegation {
	out := make([]customerSpendDelegation, 0, len(rows))
	for _, row := range rows {
		delegation := customerSpendDelegation{
			Scope:    budgets.NormalizeScope(row.Scope),
			ScopeKey: row.ScopeKey,
			Windows:  row.Windows,
		}
		if delegation.Scope == budgets.ScopeRole {
			delegation.RoleID = row.ScopeKey
		}
		out = append(out, delegation)
	}
	sort.Slice(out, func(i, j int) bool {
		return spendDelegationKey(out[i].Scope, out[i].ScopeKey) < spendDelegationKey(out[j].Scope, out[j].ScopeKey)
	})
	return out
}

func spendDelegationKey(scope, scopeKey string) string {
	return budgets.NormalizeScope(scope) + "\x00" + strings.TrimSpace(scopeKey)
}
