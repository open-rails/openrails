package embed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// localClient adapts pkg/service.Service to openrails.Client (#338).
//
// EVERY method transcribes the wire→service mapping of its standalone HTTP
// handler (cited per method, all in internal/http/handlers/) — argument
// parsing/defaulting, validation order, AND the error→status mapping — so the
// embedded transport is observably identical to openrails.NewRemote hitting the
// same engine. Errors are built through openrails.NewStatusError with the exact
// status+message the handler would have written, which is what makes errors.Is
// behave identically across transports.
//
// The service-token tenant-subject scope gate (requireServiceCustomerScope)
// is intentionally NOT transcribed: in embedded mode the host IS the principal
// and is trusted for its own tenant, exactly like a tenant-wide service token.
type localClient struct {
	svc *billingservice.Service
	// currency is the default currency applied to Authorize/Balance/window
	// requests when the request leaves it empty — the client-side counterpart of
	// openrails.WithCurrency on the remote (#476).
	currency string
	// tenantID is the engine's construction-time bound tenant (#336/#478). The
	// unified Client path has no HTTP middleware to resolve a tenant onto the
	// context the way the standalone/gin surface does, so the adapter pins the
	// bound tenant itself (ensureTenant) — the embedded host is the principal and
	// is trusted for its own tenant. Zero when no tenant is bound (the call then
	// hard-fails downstream on merchant.Require, exactly as the standalone surface
	// would for a request with no resolved tenant).
	tenantID merchant.ID
}

// ensureTenant pins the engine's bound tenant onto ctx when the caller has not
// already resolved one. A non-zero tenant already on the context (e.g. a host
// that resolved a different tenant) is left untouched.
func (c *localClient) ensureTenant(ctx context.Context) context.Context {
	if c == nil || c.tenantID.IsZero() {
		return ctx
	}
	if _, ok := merchant.FromContext(ctx); ok {
		return ctx
	}
	return merchant.WithID(ctx, c.tenantID)
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

// bindRequiredErr mirrors internal/http/request.normaliseBindError for a
// failed `binding:"required"` field: "<lowercased field> is invalid".
func bindRequiredErr(field string) error {
	return invalidErr(strings.ToLower(field) + " is invalid")
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

// mapCreditErr transcribes the credit-route error switch shared by
// ServiceHoldCredits / ServiceWithdrawCredits / ServiceCaptureHold /
// ServiceDepositCredits: ErrInsufficientCredits → 402 "insufficient_credits",
// anything else → 500 with the handler's fallback message.
func mapCreditErr(err error, fallback string) error {
	switch {
	case errors.Is(err, billingservice.ErrInsufficientCredits):
		return openrails.NewStatusError(http.StatusPaymentRequired, "", "insufficient_credits")
	default:
		return internalErr(fallback)
	}
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

func transactionFromService(t *billingservice.CreditTransaction) *openrails.CreditTransaction {
	if t == nil {
		return nil
	}
	return &openrails.CreditTransaction{
		ID:              t.ID,
		CustomerID:      t.CustomerID,
		Invoker:         t.Invoker,
		Currency:        t.Currency,
		Amount:          t.Amount,
		BalanceAfter:    t.BalanceAfter,
		TransactionType: t.TransactionType,
		Status:          t.Status,
		Authorized:      t.Authorized,
		Captured:        t.Captured,
		Source:          t.Source,
		SourceID:        t.SourceID,
		ExpiresAt:       t.ExpiresAt,
		Description:     t.Description,
		CreatedAt:       t.CreatedAt,
		UpdatedAt:       t.UpdatedAt,
	}
}

// --- Client implementation -------------------------------------------------

// Admit transcribes handlers.ServiceAdmit + admitInputFromRequest
// (service_admission.go). All verdict outcomes — allowed, abuse (429), money
// (402), gated (403) — return (resp, nil), like the remote client.
func (c *localClient) Admit(ctx context.Context, req openrails.AdmitRequest) (*openrails.AdmitResponse, error) {
	ctx = c.ensureTenant(ctx)
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
// the embedded analogue of handlers.admitInputFromRequest, shared by Admit and
// each AdmitBatch item.
func admitInputFromSDK(req openrails.AdmitRequest, payer identity.CustomerID) billingservice.AdmitInput {
	in := billingservice.AdmitInput{
		CustomerID:      payer,
		Invoker:         strings.TrimSpace(req.Invoker),
		InvokerType:     req.InvokerType,
		Tier:            req.Tier,
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

// CaptureHold transcribes handlers.ServiceCaptureHold (service_credits.go)
// without the usage-event extension (see Capture for that).
func (c *localClient) CaptureHold(ctx context.Context, req openrails.CaptureHoldRequest) (*openrails.CreditTransaction, error) {
	ctx = c.ensureTenant(ctx)
	if req.Amount == 0 { // gin binding: amount is `binding:"required"`
		return nil, bindRequiredErr("Amount")
	}
	trx, err := c.svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{RequestID: req.RequestID, Amount: req.Amount})
	if err != nil {
		if errors.Is(err, billingservice.ErrInsufficientCredits) {
			return nil, openrails.NewStatusError(http.StatusPaymentRequired, "", "insufficient_credits")
		}
		return nil, internalErr("capture failed")
	}
	return transactionFromService(trx), nil
}

// ReleaseHold transcribes handlers.ServiceReleaseHold (service_credits.go).
// NOTE the handler maps EVERY release failure — including an unknown hold — to
// 500 "release failed"; the embedded transport mirrors that (ErrInternal, not
// ErrNotFound) so errors.Is agrees across transports.
func (c *localClient) ReleaseHold(ctx context.Context, requestID string) error {
	ctx = c.ensureTenant(ctx)
	if err := c.svc.ReleaseHold(ctx, requestID); err != nil {
		return internalErr("release failed")
	}
	return nil
}

// WithdrawCredits transcribes handlers.ServiceWithdrawCredits
// (service_credits.go).
func (c *localClient) WithdrawCredits(ctx context.Context, req openrails.WithdrawCreditsRequest) (*openrails.CreditTransaction, error) {
	ctx = c.ensureTenant(ctx)
	currency, err := requireCurrency(firstNonEmpty(req.Currency, c.currency))
	if err != nil {
		return nil, err
	}
	switch {
	case strings.TrimSpace(req.Invoker) == "":
		return nil, bindRequiredErr("Invoker")
	case req.Amount == 0:
		return nil, bindRequiredErr("Amount")
	case strings.TrimSpace(req.Source) == "":
		return nil, bindRequiredErr("Source")
	case req.SourceID == nil:
		return nil, bindRequiredErr("SourceID")
	}
	payer, err := parseCustomer(customerString(req.CustomerID), "invalid customer_id")
	if err != nil {
		return nil, err
	}
	trx, err := c.svc.WithdrawCredits(ctx, billingservice.WithdrawCreditsRequest{
		CustomerID: &payer,
		Invoker:    req.Invoker,
		Currency:   currency,
		Amount:     req.Amount,
		Source:     req.Source,
		SourceID:   req.SourceID,
	})
	if err != nil {
		return nil, mapCreditErr(err, "withdraw failed")
	}
	return transactionFromService(trx), nil
}

// DepositCredits transcribes handlers.ServiceDepositCredits
// (service_credits.go).
func (c *localClient) DepositCredits(ctx context.Context, req openrails.DepositCreditsRequest) (*openrails.CreditTransaction, error) {
	ctx = c.ensureTenant(ctx)
	currency, err := requireCurrency(firstNonEmpty(req.Currency, c.currency))
	if err != nil {
		return nil, err
	}
	switch {
	case strings.TrimSpace(req.Invoker) == "":
		return nil, bindRequiredErr("Invoker")
	case req.Amount == 0:
		return nil, bindRequiredErr("Amount")
	case strings.TrimSpace(req.Source) == "":
		return nil, bindRequiredErr("Source")
	case req.SourceID == nil:
		return nil, bindRequiredErr("SourceID")
	}
	payer, err := parseCustomer(customerString(req.CustomerID), "invalid customer_id")
	if err != nil {
		return nil, err
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		v := time.Unix(req.ExpiresAt.Unix(), 0).UTC() // wire = unix seconds
		expiresAt = &v
	}
	var description *string
	if strings.TrimSpace(req.Description) != "" { // remote omits empty description
		d := req.Description
		description = &d
	}
	trx, err := c.svc.DepositCredits(ctx, billingservice.DepositCreditsRequest{
		CustomerID:  &payer,
		Invoker:     req.Invoker,
		Currency:    currency,
		Amount:      req.Amount,
		Source:      req.Source,
		SourceID:    req.SourceID,
		ExpiresAt:   expiresAt,
		Description: description,
	})
	if err != nil {
		// #483: an unknown/invalid currency is a 400 (parity with the remote handler).
		if errors.Is(err, money.ErrBillingUnitRequired) || strings.Contains(err.Error(), "unknown currency") {
			return nil, invalidErr(err.Error())
		}
		return nil, internalErr("deposit failed")
	}
	return transactionFromService(trx), nil
}

// Capture transcribes handlers.ServiceCaptureHold (service_credits.go)
// addressed by request_id, including the #311 usage-event extension.
func (c *localClient) Capture(ctx context.Context, requestID string, capturedAmount int64, usage *openrails.CaptureUsage) error {
	ctx = c.ensureTenant(ctx)
	if strings.TrimSpace(requestID) == "" {
		return invalidErr("request_id required")
	}
	if capturedAmount == 0 { // gin binding: amount is `binding:"required"`
		return bindRequiredErr("Amount")
	}
	in := billingservice.CaptureHoldRequest{RequestID: requestID, Amount: capturedAmount}
	if usage != nil && strings.TrimSpace(usage.EventType) != "" {
		in.EventType = usage.EventType
		in.Resource = usage.Resource
		in.Metadata = usage.Metadata
		in.Source = usage.Source
		in.SourceID = usage.SourceID
	}
	if _, err := c.svc.CaptureHold(ctx, in); err != nil {
		if errors.Is(err, billingservice.ErrInsufficientCredits) {
			return openrails.NewStatusError(http.StatusPaymentRequired, "", "insufficient_credits")
		}
		return internalErr("capture failed")
	}
	return nil
}

// Release transcribes handlers.ServiceReleaseHold (service_credits.go),
// addressed by request_id.
func (c *localClient) Release(ctx context.Context, requestID string) error {
	ctx = c.ensureTenant(ctx)
	if strings.TrimSpace(requestID) == "" {
		return invalidErr("request_id required")
	}
	return c.ReleaseHold(ctx, requestID)
}

// Balance transcribes handlers.ServiceGetCreditsBalance (service_credits.go)
// using the client-level currency.
func (c *localClient) Balance(ctx context.Context, customerID string) (*openrails.BalanceResponse, error) {
	ctx = c.ensureTenant(ctx)
	currency, err := requireCurrency(c.currency)
	if err != nil {
		return nil, err
	}
	payer, err := parseCustomer(customerID, "invalid customer_id")
	if err != nil {
		return nil, err
	}
	snap, err := c.svc.GetCreditAccount(ctx, payer, currency)
	if err != nil {
		return nil, invalidErr(err.Error())
	}
	return &openrails.BalanceResponse{
		Currency:              snap.Currency,
		BillingMode:           snap.BillingMode,
		BalanceAmount:         snap.BalanceAmount,
		HeldAmount:            snap.HeldAmount,
		AvailableAmount:       snap.AvailableAmount,
		OutstandingOwedAmount: snap.OutstandingOwedAmount,
	}, nil
}

// GetCreditAccount transcribes handlers.ServiceGetCreditsBalance.
func (c *localClient) GetCreditAccount(ctx context.Context, customerID, currency string) (*openrails.CreditAccount, error) {
	ctx = c.ensureTenant(ctx)
	currency, err := requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	payer, err := parseCustomer(customerID, "invalid customer_id")
	if err != nil {
		return nil, err
	}
	snap, err := c.svc.GetCreditAccount(ctx, payer, currency)
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

// SetCreditAccountSettings transcribes handlers.ServiceSetCreditAccountSettings
// (service_credits.go) and then — like the remote client — re-reads the account
// snapshot via the balance path.
func (c *localClient) SetCreditAccountSettings(ctx context.Context, customerID, currency string, in openrails.AccountSettingsInput) (*openrails.CreditAccount, error) {
	ctx = c.ensureTenant(ctx)
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
		DefaultCreditExpiryDays:  in.DefaultCreditExpiryDays,
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
	// The handler also re-reads + returns the stored settings; the remote client
	// discards that body and fetches the account snapshot. Mirror both steps so
	// a settings-read failure surfaces identically.
	if _, err := c.svc.GetCreditAccountSettings(ctx, payer, ct); err != nil {
		return nil, invalidErr(err.Error())
	}
	return c.GetCreditAccount(ctx, customerID, currency)
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

// ListCreditTransactions transcribes
// handlers.ServiceListCustomerCreditTransactions (service_credits.go),
// producing the same {"transactions":[...],"total":N} JSON the wire carries.
func (c *localClient) ListCreditTransactions(ctx context.Context, customerID, currency string, limit int) (json.RawMessage, error) {
	ctx = c.ensureTenant(ctx)
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

// UsageRollup transcribes handlers.ServiceUsageRollup (service_credits.go).
func (c *localClient) UsageRollup(ctx context.Context, customerID, currency string, from, to time.Time, groupBy string) ([]openrails.UsageRollupRow, error) {
	ctx = c.ensureTenant(ctx)
	// gin binding: customer_id, from, to, group_by are all required.
	switch {
	case strings.TrimSpace(customerID) == "":
		return nil, bindRequiredErr("CustomerID")
	case from.UTC().Unix() == 0:
		return nil, bindRequiredErr("From")
	case to.UTC().Unix() == 0:
		return nil, bindRequiredErr("To")
	case strings.TrimSpace(groupBy) == "":
		return nil, bindRequiredErr("GroupBy")
	}
	currency, err := requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	payer, err := parseCustomer(customerID, "invalid customer_id")
	if err != nil {
		return nil, err
	}
	rows, err := c.svc.ServiceUsageRollup(ctx, billingservice.ServiceUsageRollupRequest{
		CustomerID: &payer,
		Currency:   currency,
		// The wire carries unix seconds; mirror the handler's re-derivation.
		From:    time.Unix(from.UTC().Unix(), 0).UTC(),
		To:      time.Unix(to.UTC().Unix(), 0).UTC(),
		GroupBy: groupBy,
	})
	if err != nil {
		return nil, invalidErr(err.Error())
	}
	out := make([]openrails.UsageRollupRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, openrails.UsageRollupRow{Key: r.Key, Currency: r.Currency, EventCount: r.EventCount, TotalAmount: r.TotalAmount})
	}
	return out, nil
}

// BudgetStatus transcribes handlers.ServiceGetBudget (service_admission.go).
func (c *localClient) BudgetStatus(ctx context.Context, tenantSubjectID, invokerID, currency, tier string) ([]openrails.BudgetWindow, error) {
	ctx = c.ensureTenant(ctx)
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

// SetPayerSpendLimits transcribes handlers.ServiceSetPayerSpendLimits
// (service_admission.go).
func (c *localClient) SetPayerSpendLimits(ctx context.Context, tenantSubjectID string, in openrails.PayerSpendLimitInput) error {
	ctx = c.ensureTenant(ctx)
	// An EMPTY customer_id sets the tenant-wide DEFAULT tier policy (#477,
	// the platform capacity ladder declared once); a non-empty one is a per-subject
	// override.
	var payer identity.CustomerID
	if strings.TrimSpace(tenantSubjectID) != "" {
		var err error
		payer, err = parseCustomer(tenantSubjectID, "invalid customer_id")
		if err != nil {
			return err
		}
	}
	pol := billingservice.PayerSpendLimitInput{
		Tier:           in.Tier,
		PolicyCurrency: in.PolicyCurrency,
	}
	for _, b := range in.BudgetWindows {
		pol.BudgetWindows = append(pol.BudgetWindows, billingservice.TierBudgetWindowInput{
			Key: b.Key, WindowSeconds: b.WindowSeconds, Limit: b.Limit, Currency: b.Currency,
		})
	}
	for _, b := range in.BadSpendWindows {
		pol.BadSpendWindows = append(pol.BadSpendWindows, billingservice.TierBudgetWindowInput{
			Key: b.Key, WindowSeconds: b.WindowSeconds, Limit: b.Limit, Currency: b.Currency,
		})
	}
	if err := c.svc.SetPayerSpendLimits(ctx, payer, pol); err != nil {
		return internalErr("set tier policy failed")
	}
	return nil
}

// SetTierSchedule transcribes handlers.ServiceSetTierSchedule
// (service_admission.go, #476). An empty customer_id is the tenant-wide
// default schedule (zero payer); a non-empty one is a per-subject override.
func (c *localClient) SetTierSchedule(ctx context.Context, tenantSubjectID, currency string, schedule []openrails.TierScheduleRung) error {
	ctx = c.ensureTenant(ctx)
	currency, err := requireCurrency(currency)
	if err != nil {
		return err
	}
	var payer identity.CustomerID
	if strings.TrimSpace(tenantSubjectID) != "" {
		payer, err = parseCustomer(tenantSubjectID, "invalid customer_id")
		if err != nil {
			return err
		}
	}
	rungs := make([]billingservice.TierScheduleRung, 0, len(schedule))
	for _, r := range schedule {
		rungs = append(rungs, billingservice.TierScheduleRung{
			Tier: r.Tier, MinCumulativePaidAmount: r.MinCumulativePaidAmount,
		})
	}
	if err := c.svc.SetTierSchedule(ctx, payer, currency, rungs); err != nil {
		return internalErr("set tier schedule failed")
	}
	return nil
}

// SetMerchantConfiguration transcribes handlers.ServiceSetMerchantConfiguration.
func (c *localClient) SetMerchantConfiguration(ctx context.Context, in openrails.MerchantConfigurationInput) error {
	ctx = c.ensureTenant(ctx)
	ws := make([]abuse.WastedWindow, 0, len(in.DelegatedInvokerWastedSpendWindows))
	for _, w := range in.DelegatedInvokerWastedSpendWindows {
		ws = append(ws, abuse.WastedWindow{
			Key:    w.Key,
			Window: time.Duration(w.WindowSeconds) * time.Second,
			Limit:  w.Limit,
		})
	}
	if err := c.svc.SetMerchantConfiguration(ctx, billingservice.MerchantConfiguration{
		Profile:                            merchantProfileInput(in.Profile),
		DelegatedInvokerWastedSpendWindows: ws,
	}); err != nil {
		return internalErr("set merchant configuration failed")
	}
	return nil
}

func merchantProfileInput(in *openrails.MerchantProfileInput) *models.MerchantProfileConfiguration {
	if in == nil {
		return nil
	}
	return &models.MerchantProfileConfiguration{
		DisplayName:  strings.TrimSpace(in.DisplayName),
		LogoURL:      strings.TrimSpace(in.LogoURL),
		FromEmail:    strings.TrimSpace(in.FromEmail),
		SupportURL:   strings.TrimSpace(in.SupportURL),
		SupportEmail: strings.TrimSpace(in.SupportEmail),
	}
}

// GetTier transcribes handlers.ServiceGetTier (service_admission.go, #477).
func (c *localClient) GetTier(ctx context.Context, tenantSubjectID, currency string) (string, error) {
	ctx = c.ensureTenant(ctx)
	payer, err := parseCustomer(tenantSubjectID, "invalid customer_id")
	if err != nil {
		return "", err
	}
	currency, err = requireCurrency(currency)
	if err != nil {
		return "", err
	}
	tier, terr := c.svc.GetTier(ctx, payer, currency)
	if terr != nil {
		return "", internalErr("tier lookup failed")
	}
	return tier, nil
}

// ReportWastedSpend transcribes handlers.ServiceReportWastedSpend (#497).
func (c *localClient) ReportWastedSpend(ctx context.Context, report openrails.WastedSpendReport) (*openrails.WastedSpendResponse, error) {
	ctx = c.ensureTenant(ctx)
	payer, err := parseCustomer(report.CustomerID, "invalid customer_id")
	if err != nil {
		return nil, err
	}
	currency, err := requireCurrency(report.Currency)
	if err != nil {
		return nil, err
	}
	res, err := c.svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID:  payer,
		Invoker:     report.Invoker,
		InvokerType: report.InvokerType,
		Currency:    currency,
		Amount:      report.Amount,
		Source:      report.Source,
		SourceID:    report.SourceID,
		Reason:      report.Reason,
	})
	if err != nil {
		return nil, internalErr("report wasted spend failed")
	}
	return &openrails.WastedSpendResponse{
		Currency:             res.Currency,
		PolicyCurrency:       res.PolicyCurrency,
		RecordedAmount:       res.RecordedAmount,
		PolicyRecordedAmount: res.PolicyRecordedAmount,
		ForgivenAmount:       res.ForgivenAmount,
		PolicyForgivenAmount: res.PolicyForgivenAmount,
		ChargedAmount:        res.ChargedAmount,
		PolicyChargedAmount:  res.PolicyChargedAmount,
		Action:               res.Action,
		Duplicate:            res.Duplicate,
	}, nil
}

// AbuseUsage transcribes handlers.ServiceAbuseUsage (#488).
func (c *localClient) AbuseUsage(ctx context.Context, tenantSubjectID, invoker, currency, tier string) (*openrails.AbuseUsageResponse, error) {
	ctx = c.ensureTenant(ctx)
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

// SetCreditLimit transcribes handlers.ServiceSetCreditLimit (#489).
func (c *localClient) SetCreditLimit(ctx context.Context, tenantSubjectID, currency string, creditLimit int64) error {
	ctx = c.ensureTenant(ctx)
	payer, err := parseCustomer(tenantSubjectID, "invalid customer_id")
	if err != nil {
		return err
	}
	currency, err = requireCurrency(currency)
	if err != nil {
		return err
	}
	if err := c.svc.SetCreditLimit(ctx, payer, currency, creditLimit); err != nil {
		return invalidErr(err.Error())
	}
	return nil
}

// GetCreditLimit transcribes handlers.ServiceGetCreditLimit (#489).
func (c *localClient) GetCreditLimit(ctx context.Context, tenantSubjectID, currency string) (int64, error) {
	ctx = c.ensureTenant(ctx)
	payer, err := parseCustomer(tenantSubjectID, "invalid customer_id")
	if err != nil {
		return 0, err
	}
	currency, err = requireCurrency(currency)
	if err != nil {
		return 0, err
	}
	v, gerr := c.svc.GetCreditLimit(ctx, payer, currency)
	if gerr != nil {
		return 0, invalidErr(gerr.Error())
	}
	return v, nil
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

func spendLimitWindowInputs(ws []openrails.SpendLimitWindow) []billingservice.SpendLimitWindowInput {
	out := make([]billingservice.SpendLimitWindowInput, 0, len(ws))
	for _, w := range ws {
		out = append(out, billingservice.SpendLimitWindowInput{
			Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: w.Currency,
		})
	}
	return out
}

// SetInvokerSpendLimits transcribes handlers.ServiceSetInvokerSpendLimits
// (service_admission.go, #473).
func (c *localClient) SetInvokerSpendLimits(ctx context.Context, tenantSubjectID string, in openrails.InvokerSpendLimitInput) error {
	ctx = c.ensureTenant(ctx)
	payer, err := parseCustomer(tenantSubjectID, "customer_id required")
	if err != nil {
		return err
	}
	scopeKey := strings.TrimSpace(in.ScopeKey)
	if scopeKey == "" {
		scopeKey = strings.TrimSpace(in.RoleID)
	}
	if err := c.svc.SetInvokerSpendLimits(ctx, payer, billingservice.InvokerSpendLimitInput{
		Scope:    in.Scope,
		ScopeKey: scopeKey,
		Windows:  spendLimitWindowInputs(in.Windows),
	}); err != nil {
		return internalErr("set subject budget policy failed")
	}
	return nil
}

// InvokerSpendLimits transcribes handlers.ServiceGetInvokerSpendLimits
// (service_admission.go, #473): subject-owned policies only; ScopeKey maps back
// onto role_id.
func (c *localClient) InvokerSpendLimits(ctx context.Context, tenantSubjectID string) ([]openrails.InvokerSpendLimit, error) {
	ctx = c.ensureTenant(ctx)
	payer, err := parseCustomer(tenantSubjectID, "customer_id required")
	if err != nil {
		return nil, err
	}
	policies, err := c.svc.InvokerSpendLimits(ctx, payer)
	if err != nil {
		return nil, internalErr("subject budget policies lookup failed")
	}
	out := make([]openrails.InvokerSpendLimit, 0, len(policies))
	for _, p := range policies {
		ws := make([]openrails.SpendLimitWindow, 0, len(p.Windows))
		for _, w := range p.Windows {
			ws = append(ws, openrails.SpendLimitWindow{Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: w.Currency})
		}
		out = append(out, openrails.InvokerSpendLimit{Scope: p.Scope, RoleID: p.ScopeKey, Windows: ws})
	}
	return out, nil
}

func wireEntitlementRecords(recs []billingservice.EntitlementRecord) []openrails.EntitlementRecord {
	out := make([]openrails.EntitlementRecord, 0, len(recs))
	for _, e := range recs {
		rec := openrails.EntitlementRecord{
			ID:           e.ID.String(),
			CustomerID:   e.CustomerID.String(),
			Entitlement:  e.Entitlement,
			StartAt:      e.StartAt,
			EndAt:        e.EndAt,
			SourceType:   e.SourceType,
			RevokedAt:    e.RevokedAt,
			RevokeReason: e.RevokeReason,
			CreatedAt:    e.CreatedAt,
			UpdatedAt:    e.UpdatedAt,
		}
		if e.SourceID != nil {
			sourceStr := e.SourceID.String()
			rec.SourceID = &sourceStr
		}
		out = append(out, rec)
	}
	return out
}

// ListActiveEntitlements transcribes
// handlers.ServiceGetExternalSubjectEntitlements (entitlements.go): trim +
// dedupe, cap, one query, an entry per requested subject. The service-token
// scope gate is not transcribed — embedded hosts are the principal.
func (c *localClient) ListActiveEntitlements(ctx context.Context, issuer string, subjects []string, at time.Time) (map[string][]openrails.EntitlementRecord, error) {
	ctx = c.ensureTenant(ctx)
	if strings.TrimSpace(issuer) == "" {
		return nil, invalidErr("issuer required")
	}
	deduped := make([]string, 0, len(subjects))
	seen := make(map[string]struct{}, len(subjects))
	for _, s := range subjects {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		deduped = append(deduped, s)
	}
	if len(deduped) == 0 {
		return nil, invalidErr("subjects required")
	}
	if len(deduped) > billingservice.EntitlementsBatchMaxSubjects {
		return nil, invalidErr(fmt.Sprintf("too many subjects: %d > %d per call", len(deduped), billingservice.EntitlementsBatchMaxSubjects))
	}
	grouped, err := c.svc.ListActiveEntitlementRecordsByExternalSubjects(ctx, strings.TrimSpace(issuer), deduped, at)
	if err != nil {
		return nil, internalErr("failed to fetch entitlements")
	}
	out := make(map[string][]openrails.EntitlementRecord, len(deduped))
	for _, s := range deduped {
		out[s] = []openrails.EntitlementRecord{}
	}
	for subject, recs := range grouped {
		out[subject] = wireEntitlementRecords(recs)
	}
	return out, nil
}

// ResourceRevenueDaily transcribes handlers.ServiceResourceRevenue
// (service_credits.go), including the handler-side total summation.
func (c *localClient) ResourceRevenueDaily(ctx context.Context, resource, currency string, fromUnix, toUnix int64) (*openrails.ResourceRevenueResponse, error) {
	ctx = c.ensureTenant(ctx)
	resource = strings.TrimSpace(resource) // the remote client trims before sending
	// gin binding: resource, from, to are all required.
	switch {
	case resource == "":
		return nil, bindRequiredErr("Resource")
	case fromUnix == 0:
		return nil, bindRequiredErr("From")
	case toUnix == 0:
		return nil, bindRequiredErr("To")
	}
	currency, err := requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	rows, err := c.svc.ResourceRevenueDaily(ctx, resource, currency, time.Unix(fromUnix, 0).UTC(), time.Unix(toUnix, 0).UTC())
	if err != nil {
		return nil, invalidErr(err.Error())
	}
	out := &openrails.ResourceRevenueResponse{Currency: currency, Daily: make([]openrails.ResourceRevenueDailyRow, 0, len(rows))}
	for _, r := range rows {
		out.RevenueAmount += r.Amount
		out.Daily = append(out.Daily, openrails.ResourceRevenueDailyRow{Date: r.Date, Currency: r.Currency, Amount: r.Amount})
	}
	return out, nil
}

// maxAdmitBatchItems transcribes the handler bound on one admit batch
// (service_admission.go).
const maxAdmitBatchItems = 1000

// AdmitBatch transcribes handlers.ServiceAdmitBatch +
// serviceAdmitBatchVerdicts (service_admission.go, #335) with full per-item
// isolation. The per-item service-token tenant-subject scope gate is not
// transcribed — in embedded mode the host IS the principal, exactly like a
// tenant-wide token (so its `allows` is always true).
func (c *localClient) AdmitBatch(ctx context.Context, items []openrails.AdmitRequest) ([]openrails.AdmitBatchVerdict, error) {
	ctx = c.ensureTenant(ctx)
	if len(items) == 0 {
		return nil, invalidErr("items required")
	}
	if len(items) > maxAdmitBatchItems {
		return nil, invalidErr("too many items")
	}
	out := make([]openrails.AdmitBatchVerdict, len(items))
	for i, item := range items {
		if item.EstimatedAmount < 0 {
			out[i] = openrails.AdmitBatchVerdict{Status: http.StatusBadRequest, Error: "estimated_amount must be >= 0"}
			continue
		}
		payer, perr := parseCustomer(item.CustomerID, "customer_id required")
		if perr != nil {
			out[i] = openrails.AdmitBatchVerdict{Status: http.StatusBadRequest, Error: "customer_id required"}
			continue
		}
		currency, cerr := requireCurrency(item.Currency)
		if cerr != nil {
			out[i] = openrails.AdmitBatchVerdict{Status: http.StatusBadRequest, Error: "currency required"}
			continue
		}
		item.Currency = currency
		res, err := c.svc.Admit(ctx, admitInputFromSDK(item, payer))
		if err != nil {
			out[i] = openrails.AdmitBatchVerdict{Status: http.StatusInternalServerError, Error: "admission check failed"}
			continue
		}
		out[i] = openrails.AdmitBatchVerdict{Status: admitVerdictStatus(res), Result: admitResponseFromResult(res)}
	}
	return out, nil
}

// admitVerdictStatus transcribes handlers.admitVerdictStatus: the HTTP status
// the single admit route returns for this decision.
func admitVerdictStatus(res *billingservice.AdmitResult) int {
	if res.Allowed {
		return http.StatusOK
	}
	switch res.BlockedBy {
	case "abuse":
		return http.StatusTooManyRequests
	case "money":
		return http.StatusPaymentRequired
	default:
		return http.StatusForbidden
	}
}

func customerString(p *openrails.CustomerID) string {
	if p == nil || p.IsZero() {
		return ""
	}
	return p.String()
}
