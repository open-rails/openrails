package embed

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// SingleAdmitter is the embedded-only single-Admit extra (no /v1/merchant wire
// counterpart, #666 kept the batch route as the surviving HTTP surface). The
// value returned by Runtime.Client always implements it via *localClient; hosts
// that need single-Admit semantics reach it by type-asserting the
// openrails.Client to SingleAdmitter.
type SingleAdmitter interface {
	Admit(ctx context.Context, req openrails.AdmitRequest) (*openrails.AdmitResponse, error)
}

// localClient is the PERMANENT remainder of the pre-#685 handler-transcribing
// adapter. Every openrails.Client interface method except customer delegation
// writes runs through the
// unified remote implementation over the in-process transport (see
// unifiedClient in embed.go); what stays here is:
//
//   - SetCustomerSpendDelegation(s) — PERMANENTLY transcribed (#685 tail
//     decision). Its wire surface is the delegated customer-treasury family
//     (/v1/customers/*), whose gate semantics are "the credential IS the
//     customer": CustomerScopeRequired matches :customer_id against the
//     delegated principal's OWN resolved identity and the handler derives the
//     payer FROM the principal (a body customer_id is rejected). The host
//     principal is the MERCHANT — one fixed identity — while this method takes
//     an arbitrary customerID per call; routing it in-process would require
//     synthesizing a per-request ResolvedDelegated whose identity is copied
//     from the very path segment the gate exists to check (fabrication, not
//     auth), with a merchant.ID holding a non-merchant UUID. The wire also
//     deliberately offers NO merchant-credential route for this operation
//     (#666 retired the merchant-side service routes; every /v1/customers
//     route is gated on customer:* perms because the balance may be shared) —
//     an in-process mapping would mint embedded-only authority with no
//     standalone equivalent. The transcription states what this really is:
//     direct engine access by the process owner.
//   - Embedded-only extra with NO wire counterpart: single Admit (the batch
//     route is the surviving HTTP surface, #666), reachable via type assertion
//     to SingleAdmitter by hosts that need it. (SetCreditAccountSettings, ListCreditTransactions,
//     BudgetStatus, AbuseUsage were deleted in the #685 tail — no host used
//     them.)
type localClient struct {
	svc *billingservice.Service
	// rt carries the bound merchant: the transcription bypasses the in-process
	// transport, so it must pin merchant ctx itself (see merchantCtx).
	rt *app.Runtime
	// currency is the default currency the unified client is built with
	// (openrails.WithCurrency on the in-process remote).
	currency string
	// remoteOpts are host-supplied openrails.RemoteOption values (#767,
	// WithRemoteOptions) applied to the in-process remote AFTER Runtime.Client's
	// built-ins, so a host can override any remote knob (e.g. WithTimeout).
	remoteOpts []openrails.RemoteOption
}

// merchantCtx replicates the transport's merchant pinning (transport.go) for
// the transcribed path: live-read the bound merchant (provisioning may bind it
// after New), fall back to a per-call ctx pin while unbound, and stamp it
// before any merchant-owned DB access. Without this every store call fails
// merchant.Require in embedded mode.
//
// #772: an explicit per-call pin (openrails.WithMerchant) that disagrees with
// an already-bound merchant is refused rather than silently overridden by the
// bound one — one embedded engine serves one merchant.
func (c *localClient) merchantCtx(ctx context.Context) (context.Context, error) {
	mid := c.rt.ConfiguredMerchant()
	if v, ok := merchant.FromContext(ctx); ok {
		if !mid.IsZero() && v != mid {
			return ctx, openrails.NewStatusError(http.StatusConflict, "", merchantMismatchMsg(mid, v))
		}
		if mid.IsZero() {
			mid = v
		}
	}
	if !mid.IsZero() {
		ctx = merchant.WithID(ctx, mid)
	}
	return ctx, nil
}

// merchantMismatchMsg is the shared error text for both merchant-pinning
// paths (transport.go RoundTrip and this file's merchantCtx): an engine bound
// to one merchant received a call explicitly pinned (openrails.WithMerchant)
// to a DIFFERENT merchant (#772).
func merchantMismatchMsg(bound, pinned merchant.ID) string {
	return fmt.Sprintf("openrails embed: call pinned to merchant %s but engine is bound to merchant %s; one embedded engine serves one merchant", pinned, bound)
}

// ClientOption configures Runtime.Client.
type ClientOption func(*localClient)

// WithCurrency mirrors openrails.WithCurrency for the embedded client.
func WithCurrency(currency string) ClientOption {
	return func(c *localClient) { c.currency = strings.TrimSpace(currency) }
}

// WithRemoteOptions passes openrails.RemoteOption values straight through to
// the in-process remote Runtime.Client builds, applied AFTER its built-ins
// (transport, token provider, currency, timeout) — so a host can override any
// remote knob (e.g. reinstate a per-call deadline via openrails.WithTimeout)
// without embed re-exposing each one individually (#767).
func WithRemoteOptions(opts ...openrails.RemoteOption) ClientOption {
	return func(c *localClient) { c.remoteOpts = append(c.remoteOpts, opts...) }
}

// --- shared transcription helpers -----------------------------------------

func invalidErr(msg string) error {
	return openrails.NewStatusError(http.StatusBadRequest, "", msg)
}

func requireCurrency(raw string) (string, error) {
	currency := strings.TrimSpace(raw)
	if currency == "" {
		return "", invalidErr("currency required")
	}
	if money.IsQualifiedUnit(currency) {
		return currency, nil
	}
	return money.NormalizeCurrency(currency), nil
}

// wrapInternalErr wraps a 500 StatusError, appending the underlying cause: these
// StatusErrors never leave the process (in-process embedded path only), so
// folding the cause into the message is safe and needed for host debugging.
func wrapInternalErr(msg string, cause error) error {
	return openrails.NewStatusError(http.StatusInternalServerError, "", fmt.Sprintf("%s: %v", msg, cause))
}

// parseCustomer transcribes handlers.parseServiceCustomerID +
// the per-handler nil/err handling. invalidMsg is the message the handler uses
// for an unparseable id; an empty/missing id is always "customer_id
// required".
func parseCustomer(raw, invalidMsg string) (identity.CustomerID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return identity.CustomerID{}, invalidErr("customer_id required")
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return identity.CustomerID{}, invalidErr(invalidMsg)
	}
	return identity.CustomerID(id), nil
}

func budgetWindowFromDTO(w billingservice.AdmitBudgetWindowDTO) openrails.BudgetWindow {
	return openrails.BudgetWindow{
		Key:               w.Key,
		Currency:          w.Currency,
		Limit:             w.Limit,
		Used:              w.Used,
		Reserved:          w.Reserved,
		Remaining:         w.Remaining,
		ResetAfterSeconds: w.ResetAfterSeconds,
		ResetAt:           w.ResetAt,
		Allowed:           w.Allowed,
	}
}

// --- embedded-only surface ---------------------------------------------------

// Admit carries the single-admit semantics of the retired handlers.ServiceAdmit
// (#666; the batch /admissions route is the surviving HTTP surface). All verdict
// outcomes — allowed, abuse (429), money (402), gated (403) — return
// (resp, nil), like the remote client. Admit implements SingleAdmitter.
// Bypasses the in-process transport like the rest of localClient, so it must
// pin the bound merchant itself (merchantCtx) or every call fails
// merchant.Require.
func (c *localClient) Admit(ctx context.Context, req openrails.AdmitRequest) (*openrails.AdmitResponse, error) {
	if req.EstimatedAmount < 0 {
		return nil, invalidErr("estimated_amount must be >= 0")
	}
	payer, err := parseCustomer(req.CustomerID, "customer_id required")
	if err != nil {
		return nil, err
	}
	currency, err := requireCurrency(req.Currency)
	if err != nil {
		return nil, err
	}
	req.Currency = currency

	mctx, err := c.merchantCtx(ctx)
	if err != nil {
		return nil, err
	}
	res, err := c.svc.Admit(mctx, admitInputFromSDK(req, payer))
	if err != nil {
		return nil, wrapInternalErr("admission check failed", err)
	}
	return admitResponseFromResult(res), nil
}

// admitInputFromSDK maps one SDK admit request onto the service-facade input —
// the embedded analogue of handlers.admitInputFromRequest.
func admitInputFromSDK(req openrails.AdmitRequest, payer identity.CustomerID) billingservice.AdmitInput {
	trustLevel := strings.TrimSpace(req.TrustLevel)
	in := billingservice.AdmitInput{
		CustomerID:      payer,
		Invoker:         strings.TrimSpace(req.Invoker),
		InvokerType:     req.InvokerType,
		TrustLevel:      trustLevel,
		Resource:        req.Resource,
		Currency:        req.Currency,
		EstimatedAmount: req.EstimatedAmount,
		Source:          req.Source,
		SourceID:        req.RequestID,
		Roles:           req.Roles,
	}
	if req.ExpiresAt != nil {
		in.ExpiresAtUnix = *req.ExpiresAt
	}
	return in
}

func admitResponseFromResult(res *billingservice.AdmitResult) *openrails.AdmitResponse {
	out := &openrails.AdmitResponse{
		Allowed:             res.Allowed,
		Currency:            res.Currency,
		EstimatedAmount:     res.EstimatedAmount,
		PolicyCurrency:      res.PolicyCurrency,
		PolicyAmount:        res.PolicyAmount,
		StartCapacityAmount: res.StartCapacityAmount,
		BlockedBy:           res.BlockedBy,
		DenyCode:            res.DenyCode,
		RetryAfterSeconds:   res.RetryAfterSeconds,
		HoldExpiresAt:       res.HoldExpiresAt,
		BudgetReservationID: res.BudgetReservationID,
	}
	for _, w := range res.BudgetWindows {
		out.BudgetWindows = append(out.BudgetWindows, budgetWindowFromDTO(w))
	}
	return out
}

// SetCustomerSpendDelegations transcribes the customer treasury spend-delegations
// replace operation for embedded hosts (#567). PERMANENTLY transcribed — see the
// localClient doc: the /v1/customers/* gate requires a credential that IS the
// customer, which a host (merchant) principal cannot honestly satisfy for an
// arbitrary customerID; the wire deliberately has no merchant-credential
// equivalent for this operation.
func (c *localClient) SetCustomerSpendDelegations(ctx context.Context, customerID string, delegations []openrails.SpendDelegationInput) error {
	payer, err := parseCustomer(customerID, "invalid customer_id")
	if err != nil {
		return err
	}
	next := make([]billingservice.InvokerSpendLimitInput, 0, len(delegations))
	for _, d := range delegations {
		in, err := spendDelegationInput(d)
		if err != nil {
			return err
		}
		next = append(next, in)
	}
	mctx, err := c.merchantCtx(ctx)
	if err != nil {
		return err
	}
	if err := c.svc.ReplaceInvokerSpendLimits(mctx, payer, next); err != nil {
		return wrapInternalErr("set customer spend delegations failed", err)
	}
	return nil
}

// SetCustomerSpendDelegation uses the service/store single-row upsert path;
// unlike SetCustomerSpendDelegations it never loads or deletes sibling rows.
func (c *localClient) SetCustomerSpendDelegation(ctx context.Context, customerID string, delegation openrails.SpendDelegationInput) error {
	payer, err := parseCustomer(customerID, "invalid customer_id")
	if err != nil {
		return err
	}
	in, err := spendDelegationInput(delegation)
	if err != nil {
		return err
	}
	mctx, err := c.merchantCtx(ctx)
	if err != nil {
		return err
	}
	if err := c.svc.SetInvokerSpendLimits(mctx, payer, in); err != nil {
		return wrapInternalErr("set customer spend delegation failed", err)
	}
	return nil
}

func spendDelegationInput(d openrails.SpendDelegationInput) (billingservice.InvokerSpendLimitInput, error) {
	scope := budgets.NormalizeScope(d.Scope)
	scopeKey := strings.TrimSpace(d.ScopeKey)
	roleID := strings.TrimSpace(d.RoleID)
	if roleID != "" && scope != budgets.ScopeRole {
		return billingservice.InvokerSpendLimitInput{}, invalidErr(fmt.Sprintf("role_id is only allowed for scope %q", budgets.ScopeRole))
	}
	if scopeKey != "" && roleID != "" && scopeKey != roleID {
		return billingservice.InvokerSpendLimitInput{}, invalidErr("role_id must match scope_key")
	}
	if scopeKey == "" {
		scopeKey = roleID
	}
	windows := make([]models.BudgetWindowPolicy, 0, len(d.Windows))
	for _, w := range d.Windows {
		windows = append(windows, models.BudgetWindowPolicy{
			Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: w.Currency,
		})
	}
	normalized, err := admission.ValidateInvokerSpendLimit(admission.InvokerSpendLimit{
		Scope: scope, ScopeKey: scopeKey, Windows: windows,
	})
	if err != nil {
		return billingservice.InvokerSpendLimitInput{}, invalidErr(err.Error())
	}
	out := billingservice.InvokerSpendLimitInput{Scope: normalized.Scope, ScopeKey: normalized.ScopeKey}
	for _, w := range normalized.Windows {
		out.Windows = append(out.Windows, billingservice.SpendLimitWindowInput{
			Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: w.Currency,
		})
	}
	return out, nil
}
