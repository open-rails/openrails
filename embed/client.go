package embed

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/app"
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

// localClient is the direct embedded remainder of the pre-#685 adapter. Most
// openrails.Client methods run through the unified remote implementation over
// the in-process transport (see unifiedClient in embed.go); what stays here is:
//
//   - SetCustomerSpendDelegation(s), implemented directly through pkg/service.
//     Standalone mode exposes equivalent merchant-authenticated service routes;
//     `/v1/customers/*` remains the distinct customer-owned browser surface.
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

// merchantScope is what the transcribed path must use instead of a bare
// merchantCtx: it resolves the merchant AND pins a merchant-scoped DB
// connection for the duration of fn — the transcription's equivalent of the
// MerchantDBConnMW the in-process transport gets for free.
//
// or#868 B3: the transcribed methods (Admit, SetCustomerSpendDelegation(s))
// bypass the transport, so they only ever set merchant.WithID — a context
// VALUE. A value does not scope a database. Under the production
// openrails_app role the payable-customer materialization inside Admit was
// denied outright ("new row violates row-level security policy for table
// customers", 42501), i.e. the embedded seam's admission surface 500'd for
// every host — doujins, hentai0 and cozy-art all consume it. Pinning here
// fixes the whole transcribed surface at its one shared entry point rather
// than per handler.
func (c *localClient) merchantScope(ctx context.Context, fn func(ctx context.Context) error) error {
	mctx, err := c.merchantCtx(ctx)
	if err != nil {
		return err
	}
	if c.rt == nil || c.rt.DB == nil {
		return fn(mctx)
	}
	return c.rt.DB.RunInMerchantConn(mctx, fn)
}

// merchantCtx replicates the transport's merchant pinning (transport.go) for
// the transcribed path: live-read the bound merchant (provisioning may bind it
// after New), fall back to a per-call ctx pin while unbound, and stamp it
// before any merchant-owned DB access. Without this every store call fails
// merchant.Require in embedded mode. It resolves the merchant only — callers
// that touch the database go through merchantScope, which also pins the
// connection carrying the RLS GUC.
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

// statusErr reports whether err is already a wire-shaped refusal (the #772
// merchant-pin mismatch, raised inside merchantScope) that must reach the host
// verbatim instead of being re-wrapped as an internal error.
func statusErr(err error) bool {
	var se *openrails.StatusError
	return errors.As(err, &se)
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

	var res *billingservice.AdmitResult
	if err := c.merchantScope(ctx, func(ctx context.Context) error {
		var e error
		res, e = c.svc.Admit(ctx, admitInputFromSDK(req, payer))
		return e
	}); err != nil {
		if statusErr(err) {
			return nil, err
		}
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
		StartCapacityAmount: res.StartCapacityAmount,
		BlockedBy:           res.BlockedBy,
		DenyCode:            res.DenyCode,
		RetryAfterSeconds:   res.RetryAfterSeconds,
		HoldExpiresAt:       res.HoldExpiresAt,
	}
	return out
}

// SetCustomerSpendDelegations applies the spend-delegation replacement directly
// for embedded hosts. Standalone callers reach the equivalent merchant-machine
// route; both paths pin the merchant and share the same service/store semantics.
func (c *localClient) SetCustomerSpendDelegations(ctx context.Context, customerID string, delegations []openrails.SpendDelegationInput) error {
	payer, err := parseCustomer(customerID, "invalid customer_id")
	if err != nil {
		return err
	}
	next := make([]billingservice.InvokerSpendLimitInput, 0, len(delegations))
	for _, d := range delegations {
		next = append(next, spendDelegationInput(d))
	}
	if err := c.merchantScope(ctx, func(ctx context.Context) error {
		return c.svc.ReplaceInvokerSpendLimits(ctx, payer, next)
	}); err != nil {
		if statusErr(err) {
			return err
		}
		return spendDelegationWriteError("set customer spend delegations failed", err)
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
	in := spendDelegationInput(delegation)
	if err := c.merchantScope(ctx, func(ctx context.Context) error {
		return c.svc.SetInvokerSpendLimits(ctx, payer, in)
	}); err != nil {
		if statusErr(err) {
			return err
		}
		return spendDelegationWriteError("set customer spend delegation failed", err)
	}
	return nil
}

func spendDelegationInput(d openrails.SpendDelegationInput) billingservice.InvokerSpendLimitInput {
	out := billingservice.InvokerSpendLimitInput{
		Scope: d.Scope, ScopeKey: d.ScopeKey, RoleID: d.RoleID,
		Windows: make([]billingservice.SpendLimitWindowInput, 0, len(d.Windows)),
	}
	for _, w := range d.Windows {
		out.Windows = append(out.Windows, billingservice.SpendLimitWindowInput{
			Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: w.Currency,
		})
	}
	return out
}

func spendDelegationWriteError(message string, err error) error {
	if errors.Is(err, billingservice.ErrInvalidInvokerSpendLimit) {
		return invalidErr(err.Error())
	}
	return wrapInternalErr(message, err)
}
