package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// localClient is the REMAINDER of the pre-#685 handler-transcribing adapter.
// Every openrails.Client interface method now runs through the unified remote
// implementation over the in-process transport (see unifiedClient in embed.go);
// what stays here is:
//
//   - SetCustomerSpendDelegations — its wire surface is the delegated
//     customer-treasury family (/v1/customers/*), not the merchant API the
//     in-process transport serves; migrate it when that surface is routable
//     in-process.
//   - Embedded-only extras with NO wire counterpart (single Admit — the batch
//     route is the surviving HTTP surface, #666 — SetCreditAccountSettings,
//     ListCreditTransactions, BudgetStatus, AbuseUsage), reachable via type
//     assertion by hosts that need them.
type localClient struct {
	svc *billingservice.Service
	// currency is the default currency the unified client is built with
	// (openrails.WithCurrency on the in-process remote).
	currency string
}

// ClientOption configures Runtime.Client.
type ClientOption func(*localClient)

// WithCurrency mirrors openrails.WithCurrency for the embedded client.
func WithCurrency(currency string) ClientOption {
	return func(c *localClient) { c.currency = strings.TrimSpace(currency) }
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func internalErr(msg string) error {
	return openrails.NewStatusError(http.StatusInternalServerError, "", msg)
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
// (resp, nil), like the remote client.
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

	res, err := c.svc.Admit(ctx, admitInputFromSDK(req, payer))
	if err != nil {
		return nil, internalErr("admission check failed")
	}
	return admitResponseFromResult(res), nil
}

// admitInputFromSDK maps one SDK admit request onto the service-facade input —
// the embedded analogue of handlers.admitInputFromRequest.
func admitInputFromSDK(req openrails.AdmitRequest, payer identity.CustomerID) billingservice.AdmitInput {
	tier := strings.TrimSpace(req.TrustTier)
	if tier == "" {
		tier = strings.TrimSpace(req.Tier)
	}
	in := billingservice.AdmitInput{
		CustomerID:      payer,
		Invoker:         strings.TrimSpace(req.Invoker),
		InvokerType:     req.InvokerType,
		Tier:            tier,
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

// SetCreditAccountSettings carries the semantics of the retired
// handlers.ServiceSetCreditAccountSettings (#666) and then — like the remote
// client did — re-reads the account snapshot via the balance path.
func (c *localClient) SetCreditAccountSettings(ctx context.Context, customerID, currency string, in openrails.AccountSettingsInput) (*openrails.CreditAccount, error) {
	payer, err := parseCustomer(customerID, "customer_id required")
	if err != nil {
		return nil, err
	}
	ct, err := requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	settings := money.AccountSettingsInput{
		BillingMode:              in.BillingMode,
		MaxSpendPerDay:           in.MaxSpendPerDay,
		MaxSpendPerMonth:         in.MaxSpendPerMonth,
		MaxOutstandingOwedAmount: in.MaxOutstandingOwedAmount,
		LowBalanceThreshold:      in.LowBalanceThreshold,
		AutoTopupEnabled:         in.AutoTopupEnabled,
		AutoTopupAmountCents:     in.AutoTopupAmountCents,
		DefaultCreditExpiryHours: in.DefaultCreditExpiryHours,
		HardStopOnBreach:         in.HardStopOnBreach,
		AlertThresholdPct:        in.AlertThresholdPct,
	}
	if in.AutoTopupPaymentMethod != nil {
		if pm, perr := uuid.Parse(strings.TrimSpace(*in.AutoTopupPaymentMethod)); perr == nil {
			settings.AutoTopupPaymentMethod = &pm
		}
	}
	if err := c.svc.SetCreditAccountSettings(ctx, payer, ct, settings); err != nil {
		return nil, invalidErr(err.Error())
	}
	// The handler also re-read + returned the stored settings; mirror both steps
	// so a settings-read failure surfaces identically.
	if _, err := c.svc.GetCreditAccountSettings(ctx, payer, ct); err != nil {
		return nil, invalidErr(err.Error())
	}
	snap, err := c.svc.GetCreditAccount(ctx, payer, ct)
	if err != nil {
		return nil, invalidErr(err.Error())
	}
	return &openrails.CreditAccount{
		CustomerID:            snap.CustomerID.String(),
		Currency:              snap.Currency,
		BillingMode:           snap.BillingMode,
		BalanceAmount:         snap.BalanceAmount,
		HeldAmount:            snap.HeldAmount,
		AvailableAmount:       snap.AvailableAmount,
		OutstandingOwedAmount: snap.OutstandingOwedAmount,
	}, nil
}

// serviceTxn mirrors the handler's serviceTxnResponse wire shape
// (service_credits.go) for ListCreditTransactions passthrough JSON.
type serviceTxn struct {
	ID              uuid.UUID `json:"id"`
	CustomerID      uuid.UUID `json:"customer_id"`
	Invoker         string    `json:"invoker"`
	Currency        string    `json:"currency"`
	Amount          int64     `json:"amount"`
	TransactionType string    `json:"transaction_type"`
	Status          string    `json:"status"`
	Source          string    `json:"source"`
	CreatedAt       time.Time `json:"created_at"`
}

// ListCreditTransactions carries the semantics of the retired
// handlers.ServiceListCustomerCreditTransactions (#666), producing the same
// {"transactions":[...],"total":N} JSON the wire carried.
func (c *localClient) ListCreditTransactions(ctx context.Context, customerID, currency string, limit int) (json.RawMessage, error) {
	currency, err := requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	payer, err := parseCustomer(customerID, "customer_id required")
	if err != nil {
		return nil, err
	}
	items, total, err := c.svc.GetCustomerCreditTransactions(ctx, payer, currency, limit, 0)
	if err != nil {
		return nil, invalidErr(err.Error())
	}
	out := make([]serviceTxn, 0, len(items))
	for _, t := range items {
		out = append(out, serviceTxn{
			ID: t.ID, CustomerID: t.CustomerID, Invoker: t.Invoker, Currency: t.Currency, Amount: t.Amount,
			TransactionType: t.TransactionType, Status: t.Status, Source: t.Source,
			CreatedAt: t.CreatedAt,
		})
	}
	raw, merr := json.Marshal(map[string]any{"transactions": out, "total": total})
	if merr != nil {
		return nil, internalErr(merr.Error())
	}
	return raw, nil
}

// BudgetStatus carries the semantics of the retired handlers.ServiceGetBudget (#666).
func (c *localClient) BudgetStatus(ctx context.Context, tenantSubjectID, invokerID, currency, tier string) ([]openrails.BudgetWindow, error) {
	payer, err := parseCustomer(tenantSubjectID, "customer_id required")
	if err != nil {
		return nil, err
	}
	currency, err = requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	statuses, err := c.svc.BudgetStatus(ctx, payer, invokerID, currency, tier)
	if err != nil {
		return nil, internalErr("budget lookup failed")
	}
	out := make([]openrails.BudgetWindow, 0, len(statuses))
	for _, w := range statuses {
		out = append(out, budgetWindowFromDTO(w))
	}
	return out, nil
}

// SetCustomerSpendDelegations transcribes the customer treasury spend-delegations
// replace operation for embedded hosts (#567). NOT yet migrated to the
// in-process transport: its wire surface is /v1/customers/* (delegated
// customer-treasury auth), not the merchant API.
func (c *localClient) SetCustomerSpendDelegations(ctx context.Context, customerID string, delegations []openrails.SpendDelegationInput) error {
	payer, err := parseCustomer(customerID, "invalid customer_id")
	if err != nil {
		return err
	}
	next := make([]billingservice.InvokerSpendLimitInput, 0, len(delegations))
	for _, d := range delegations {
		windows := make([]billingservice.SpendLimitWindowInput, 0, len(d.Windows))
		for _, w := range d.Windows {
			windows = append(windows, billingservice.SpendLimitWindowInput{
				Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: w.Currency,
			})
		}
		next = append(next, billingservice.InvokerSpendLimitInput{
			Scope:    d.Scope,
			ScopeKey: firstNonEmpty(d.ScopeKey, d.RoleID),
			Windows:  windows,
		})
	}
	if err := c.svc.ReplaceInvokerSpendLimits(ctx, payer, next); err != nil {
		return internalErr("set customer spend delegations failed")
	}
	return nil
}

// AbuseUsage carries the semantics of the retired handlers.ServiceAbuseUsage (#488/#666).
func (c *localClient) AbuseUsage(ctx context.Context, tenantSubjectID, invoker, currency, tier string) (*openrails.AbuseUsageResponse, error) {
	payer, err := parseCustomer(tenantSubjectID, "invalid customer_id")
	if err != nil {
		return nil, err
	}
	currency, err = requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	pw, aw, uerr := c.svc.AbuseUsage(ctx, payer, invoker, currency, tier)
	if uerr != nil {
		return nil, internalErr("abuse usage lookup failed")
	}
	return &openrails.AbuseUsageResponse{
		Currency:       currency,
		PayerWindows:   abuseUsageWindows(pw),
		InvokerWindows: abuseUsageWindows(aw),
	}, nil
}

func abuseUsageWindows(ws []billingservice.AbuseUsageWindow) []openrails.AbuseUsageWindow {
	out := make([]openrails.AbuseUsageWindow, 0, len(ws))
	for _, w := range ws {
		out = append(out, openrails.AbuseUsageWindow{
			Key: w.Key, Currency: w.Currency, Window: w.Window, Used: w.Used,
			Limit: w.Limit, OverBudget: w.OverBudget,
		})
	}
	return out
}
