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
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
	"github.com/open-rails/openrails/pkg/merchant"
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
// The service-token tenant-subject scope gate (requireServiceMerchantSubjectScope)
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

// WithCurrency mirrors openrails.WithCurrency for the embedded client (#476).
func WithCurrency(currency string) ClientOption {
	return func(c *localClient) { c.currency = strings.TrimSpace(currency) }
}

// --- shared transcription helpers -----------------------------------------

func invalidErr(msg string) error {
	return openrails.NewStatusError(http.StatusBadRequest, "", msg)
}

func internalErr(msg string) error {
	return openrails.NewStatusError(http.StatusInternalServerError, "", msg)
}

// bindRequiredErr mirrors internal/http/request.normaliseBindError for a
// failed `binding:"required"` field: "<lowercased field> is invalid".
func bindRequiredErr(field string) error {
	return invalidErr(strings.ToLower(field) + " is invalid")
}

// parseMerchantSubject transcribes handlers.parseServiceMerchantSubjectID +
// the per-handler nil/err handling. invalidMsg is the message the handler uses
// for an unparseable id; an empty/missing id is always "tenant_subject_id
// required".
func parseMerchantSubject(raw, invalidMsg string) (identity.MerchantSubjectID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return identity.MerchantSubjectID{}, invalidErr("tenant_subject_id required")
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return identity.MerchantSubjectID{}, invalidErr(invalidMsg)
	}
	return identity.MerchantSubjectID(id), nil
}

// mapCreditErr transcribes the credit-route error switch shared by
// ServiceHoldCredits / ServiceWithdrawCredits / ServiceCaptureHold /
// ServiceDepositCredits: ErrInsufficientCredits → 402 "insufficient_credits",
// ErrCreditTypeInactive → 400 "credit_type_inactive", anything else → 500 with
// the handler's fallback message.
func mapCreditErr(err error, fallback string) error {
	switch {
	case errors.Is(err, billingservice.ErrInsufficientCredits):
		return openrails.NewStatusError(http.StatusPaymentRequired, "", "insufficient_credits")
	case errors.Is(err, billingservice.ErrCreditTypeInactive):
		return invalidErr("credit_type_inactive")
	default:
		return internalErr(fallback)
	}
}

func budgetWindowFromDTO(w billingservice.AdmitBudgetWindowDTO) openrails.BudgetWindow {
	return openrails.BudgetWindow{
		Key:               w.Key,
		Limit:             w.Limit,
		Used:              w.Used,
		Reserved:          w.Reserved,
		Remaining:         w.Remaining,
		ResetAfterSeconds: w.ResetAfterSeconds,
		ResetAt:           w.ResetAt,
		Cadence:           w.Cadence,
		Allowed:           w.Allowed,
	}
}

func transactionFromService(t *billingservice.CreditTransaction) *openrails.CreditTransaction {
	if t == nil {
		return nil
	}
	return &openrails.CreditTransaction{
		ID:              t.ID,
		MerchantSubjectID: t.MerchantSubjectID,
		Actor:           t.Actor,
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
// (service_admission.go). All verdict outcomes — allowed, throughput (429),
// money (402), gated (403) — return (resp, nil), like the remote client.
func (c *localClient) Admit(ctx context.Context, req openrails.AdmitRequest) (*openrails.AdmitResponse, error) {
	ctx = c.ensureTenant(ctx)
	if req.EstimateMicros < 0 {
		return nil, invalidErr("estimate_micros must be >= 0")
	}
	payer, err := parseMerchantSubject(req.PayerTenantID, "tenant_subject_id required")
	if err != nil {
		return nil, err
	}

	res, err := c.svc.Admit(ctx, admitInputFromSDK(req, payer))
	if err != nil {
		return nil, internalErr("admission check failed")
	}
	return admitResponseFromResult(res), nil
}

// admitInputFromSDK maps one SDK admit request onto the service-facade input —
// the embedded analogue of handlers.admitInputFromRequest, shared by Admit and
// each AdmitBatch item.
func admitInputFromSDK(req openrails.AdmitRequest, payer identity.MerchantSubjectID) billingservice.AdmitInput {
	in := billingservice.AdmitInput{
		MerchantSubjectID: payer,
		Actor:           strings.TrimSpace(req.Actor),
		Tier:            req.Tier,
		Resource:        req.Resource,
		Amounts:         req.Amounts,
		EstimateMicros:  req.EstimateMicros,
		Source:          req.Source,
		SourceID:        req.RequestID,
		Roles:           req.Roles,
	}
	if req.ExpiresAt != nil {
		in.ExpiresAtUnix = *req.ExpiresAt
	}
	for _, w := range req.TenantThroughput {
		in.TenantThroughput = append(in.TenantThroughput, billingservice.AdmitThroughputWindow{
			Unit: w.Unit, WindowSeconds: w.WindowSeconds, Max: w.Max,
		})
	}
	return in
}

func admitResponseFromResult(res *billingservice.AdmitResult) *openrails.AdmitResponse {
	out := &openrails.AdmitResponse{
		Allowed:             res.Allowed,
		BlockedBy:           res.BlockedBy,
		BlockedUnit:         res.BlockedUnit,
		DenyCode:            res.DenyCode,
		RetryAfterSeconds:   res.RetryAfterSeconds,
		ReservationID:       res.ReservationID,
		BudgetReservationID: res.BudgetReservationID,
		ResolvedTier:        res.ResolvedTier,
	}
	for _, w := range res.Windows {
		out.Windows = append(out.Windows, openrails.AdmitWindow{
			Unit: w.Unit, Limit: w.Limit, Remaining: w.Remaining, ResetAfterSeconds: w.ResetAfterSeconds,
		})
	}
	for _, w := range res.BudgetWindows {
		out.BudgetWindows = append(out.BudgetWindows, budgetWindowFromDTO(w))
	}
	return out
}

// Authorize transcribes handlers.ServiceAuthorizeCredits (service_credits.go).
func (c *localClient) Authorize(ctx context.Context, req openrails.AuthorizeRequest) (*openrails.AuthorizeResponse, error) {
	ctx = c.ensureTenant(ctx)
	if req.Currency == "" {
		req.Currency = c.currency
	}
	// gin binding: request_id is `binding:"required"`. Currency is optional (#476).
	if strings.TrimSpace(req.RequestID) == "" {
		return nil, bindRequiredErr("RequestID")
	}
	if req.EstimateMicros < 0 {
		return nil, invalidErr("estimate_micros must be >= 0")
	}
	payer, err := parseMerchantSubject(req.PayerTenantID, "invalid tenant_subject_id")
	if err != nil {
		return nil, err
	}
	var expiresAt time.Time
	if req.ExpiresAt != nil {
		expiresAt = time.Unix(*req.ExpiresAt, 0).UTC()
	}
	out, err := c.svc.AuthorizeAndHold(ctx, billingservice.AuthorizeAndHoldRequest{
		MerchantSubjectID: payer,
		Actor:           req.Actor,
		Currency:        req.Currency,
		EstimateMicros:  req.EstimateMicros,
		RequestID:       req.RequestID,
		ExpiresAt:       expiresAt,
	})
	if err != nil {
		if errors.Is(err, billingservice.ErrCreditTypeInactive) {
			return nil, invalidErr("credit_type_inactive")
		}
		return nil, internalErr("authorize failed")
	}
	resp := &openrails.AuthorizeResponse{
		Allowed:           out.Allowed,
		DenyCode:          out.DenyCode,
		BillingMode:       out.BillingMode,
		AvailableMicros:   out.AvailableMicros,
		OutstandingMicros: out.OutstandingOwedMicros,
		RetryAfterSeconds: out.RetryAfterSeconds,
	}
	if out.RemainingTodayMicros != nil {
		resp.RemainingTodayMicros = *out.RemainingTodayMicros
	}
	if out.ReservationID != nil {
		resp.ReservationID = out.ReservationID.String()
	}
	return resp, nil
}

// HoldCredits transcribes handlers.ServiceHoldCredits (service_credits.go).
func (c *localClient) HoldCredits(ctx context.Context, req openrails.HoldCreditsRequest) (*openrails.CreditHold, error) {
	ctx = c.ensureTenant(ctx)
	// gin binding (in struct field order): actor, amount, source, source_id,
	// expires_at are all `binding:"required"`. Currency is optional (#476).
	switch {
	case strings.TrimSpace(req.Actor) == "":
		return nil, bindRequiredErr("Actor")
	case req.Amount == 0:
		return nil, bindRequiredErr("Amount")
	case strings.TrimSpace(req.Source) == "":
		return nil, bindRequiredErr("Source")
	case strings.TrimSpace(req.SourceID) == "":
		return nil, bindRequiredErr("SourceID")
	}
	payer, err := parseMerchantSubject(payerString(req.PayerTenantID), "invalid tenant_subject_id")
	if err != nil {
		return nil, err
	}
	hold, err := c.svc.HoldCredits(ctx, billingservice.HoldCreditsRequest{
		MerchantSubjectID: &payer,
		Actor:           req.Actor,
		Currency:        req.Currency,
		Amount:          req.Amount,
		Source:          req.Source,
		SourceID:        req.SourceID,
		// The wire carries unix seconds; mirror the handler's re-derivation so
		// sub-second precision is dropped identically on both transports.
		ExpiresAt: time.Unix(req.ExpiresAt.Unix(), 0).UTC(),
	})
	if err != nil {
		return nil, mapCreditErr(err, "hold failed")
	}
	return &openrails.CreditHold{
		ID:        hold.ID,
		Actor:     hold.Actor,
		Amount:    hold.Amount,
		Source:    hold.Source,
		SourceID:  hold.SourceID,
		Status:    hold.Status,
		ExpiresAt: hold.ExpiresAt,
		Captured:  hold.Captured,
		CreatedAt: hold.CreatedAt,
		UpdatedAt: hold.UpdatedAt,
	}, nil
}

// CaptureHold transcribes handlers.ServiceCaptureHold (service_credits.go)
// without the usage-event extension (see Capture for that).
func (c *localClient) CaptureHold(ctx context.Context, req openrails.CaptureHoldRequest) (*openrails.CreditTransaction, error) {
	ctx = c.ensureTenant(ctx)
	if req.Amount == 0 { // gin binding: amount is `binding:"required"`
		return nil, bindRequiredErr("Amount")
	}
	trx, err := c.svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{HoldID: req.HoldID, Amount: req.Amount})
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
func (c *localClient) ReleaseHold(ctx context.Context, holdID uuid.UUID) error {
	ctx = c.ensureTenant(ctx)
	if err := c.svc.ReleaseHold(ctx, holdID); err != nil {
		return internalErr("release failed")
	}
	return nil
}

// WithdrawCredits transcribes handlers.ServiceWithdrawCredits
// (service_credits.go).
func (c *localClient) WithdrawCredits(ctx context.Context, req openrails.WithdrawCreditsRequest) (*openrails.CreditTransaction, error) {
	ctx = c.ensureTenant(ctx)
	// Currency is optional (#476).
	switch {
	case strings.TrimSpace(req.Actor) == "":
		return nil, bindRequiredErr("Actor")
	case req.Amount == 0:
		return nil, bindRequiredErr("Amount")
	case strings.TrimSpace(req.Source) == "":
		return nil, bindRequiredErr("Source")
	case req.SourceID == nil:
		return nil, bindRequiredErr("SourceID")
	}
	payer, err := parseMerchantSubject(payerString(req.PayerTenantID), "invalid tenant_subject_id")
	if err != nil {
		return nil, err
	}
	trx, err := c.svc.WithdrawCredits(ctx, billingservice.WithdrawCreditsRequest{
		MerchantSubjectID: &payer,
		Actor:           req.Actor,
		Currency:        req.Currency,
		Amount:          req.Amount,
		Source:          req.Source,
		SourceID:        req.SourceID,
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
	// Currency is optional (#476).
	switch {
	case strings.TrimSpace(req.Actor) == "":
		return nil, bindRequiredErr("Actor")
	case req.Amount == 0:
		return nil, bindRequiredErr("Amount")
	case strings.TrimSpace(req.Source) == "":
		return nil, bindRequiredErr("Source")
	case req.SourceID == nil:
		return nil, bindRequiredErr("SourceID")
	}
	payer, err := parseMerchantSubject(payerString(req.PayerTenantID), "invalid tenant_subject_id")
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
		MerchantSubjectID: &payer,
		Actor:           req.Actor,
		Currency:        req.Currency,
		Amount:          req.Amount,
		Source:          req.Source,
		SourceID:        req.SourceID,
		ExpiresAt:       expiresAt,
		Description:     description,
	})
	if err != nil {
		if errors.Is(err, billingservice.ErrCreditTypeInactive) {
			return nil, invalidErr("credit_type_inactive")
		}
		// #483: an unknown/invalid currency is a 400 (parity with the remote handler).
		if errors.Is(err, money.ErrBillingUnitRequired) || strings.Contains(err.Error(), "unknown currency") {
			return nil, invalidErr(err.Error())
		}
		return nil, internalErr("deposit failed")
	}
	return transactionFromService(trx), nil
}

// Capture transcribes handlers.ServiceCaptureHold (service_credits.go)
// addressed by reservation id, including the #311 usage-event extension.
func (c *localClient) Capture(ctx context.Context, reservationID string, capturedMicros int64, usage *openrails.CaptureUsage) error {
	ctx = c.ensureTenant(ctx)
	holdID, err := uuid.Parse(strings.TrimSpace(reservationID))
	if err != nil {
		return invalidErr("invalid hold id")
	}
	if capturedMicros == 0 { // gin binding: amount is `binding:"required"`
		return bindRequiredErr("Amount")
	}
	in := billingservice.CaptureHoldRequest{HoldID: holdID, Amount: capturedMicros}
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
// addressed by reservation id.
func (c *localClient) Release(ctx context.Context, reservationID string) error {
	ctx = c.ensureTenant(ctx)
	holdID, err := uuid.Parse(strings.TrimSpace(reservationID))
	if err != nil {
		return invalidErr("invalid hold id")
	}
	return c.ReleaseHold(ctx, holdID)
}

// Balance transcribes handlers.ServiceGetCreditsBalance (service_credits.go):
// an absent currency defaults to "USD" (#476); the remote client sends its
// WithCurrency default when set, mirrored here.
func (c *localClient) Balance(ctx context.Context, payerTenantID string) (*openrails.BalanceResponse, error) {
	ctx = c.ensureTenant(ctx)
	payer, err := parseMerchantSubject(payerTenantID, "invalid tenant_subject_id")
	if err != nil {
		return nil, err
	}
	snap, err := c.svc.GetCreditAccount(ctx, payer, c.currency)
	if err != nil {
		return nil, invalidErr(err.Error())
	}
	return &openrails.BalanceResponse{
		BillingMode:           snap.BillingMode,
		BalanceMicros:         snap.BalanceMicros,
		HeldMicros:            snap.HeldMicros,
		AvailableMicros:       snap.AvailableMicros,
		OutstandingOwedMicros: snap.OutstandingOwedMicros,
	}, nil
}

// GetCreditAccount transcribes handlers.ServiceGetCreditsBalance
// (service_credits.go) with the remote client's "USD" normalization (#476).
func (c *localClient) GetCreditAccount(ctx context.Context, payerTenantID, currency string) (*openrails.CreditAccount, error) {
	ctx = c.ensureTenant(ctx)
	payer, err := parseMerchantSubject(payerTenantID, "invalid tenant_subject_id")
	if err != nil {
		return nil, err
	}
	snap, err := c.svc.GetCreditAccount(ctx, payer, currency)
	if err != nil {
		return nil, invalidErr(err.Error())
	}
	return &openrails.CreditAccount{
		PayerTenantID:         snap.MerchantSubjectID.String(),
		Currency:              snap.Currency,
		BillingMode:           snap.BillingMode,
		BalanceMicros:         snap.BalanceMicros,
		HeldMicros:            snap.HeldMicros,
		AvailableMicros:       snap.AvailableMicros,
		OutstandingOwedMicros: snap.OutstandingOwedMicros,
	}, nil
}

// SetCreditAccountSettings transcribes handlers.ServiceSetCreditAccountSettings
// (service_credits.go) and then — like the remote client — re-reads the account
// snapshot via the balance path.
func (c *localClient) SetCreditAccountSettings(ctx context.Context, payerTenantID, currency string, in openrails.AccountSettingsInput) (*openrails.CreditAccount, error) {
	ctx = c.ensureTenant(ctx)
	payer, err := parseMerchantSubject(payerTenantID, "tenant_subject_id required")
	if err != nil {
		return nil, err
	}
	ct := strings.TrimSpace(currency)
	settings := money.AccountSettingsInput{
		BillingMode:              in.BillingMode,
		MaxSpendPerDayMicros:     in.MaxSpendPerDayMicros,
		MaxSpendPerMonthMicros:   in.MaxSpendPerMonthMicros,
		MaxOutstandingOwedMicros: in.MaxOutstandingOwedMicros,
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
	return c.GetCreditAccount(ctx, payerTenantID, currency)
}

// serviceTxn mirrors the handler's serviceTxnResponse wire shape
// (service_credits.go) for ListCreditTransactions passthrough JSON.
type serviceTxn struct {
	ID              uuid.UUID `json:"id"`
	MerchantSubjectID uuid.UUID `json:"tenant_subject_id"`
	Actor           string    `json:"actor"`
	Amount          int64     `json:"amount"`
	TransactionType string    `json:"transaction_type"`
	Status          string    `json:"status"`
	Source          string    `json:"source"`
	CreatedAt       time.Time `json:"created_at"`
}

// ListCreditTransactions transcribes
// handlers.ServiceListMerchantSubjectCreditTransactions (service_credits.go),
// producing the same {"transactions":[...],"total":N} JSON the wire carries.
func (c *localClient) ListCreditTransactions(ctx context.Context, payerTenantID, currency string, limit int) (json.RawMessage, error) {
	ctx = c.ensureTenant(ctx)
	payer, err := parseMerchantSubject(payerTenantID, "tenant_subject_id required")
	if err != nil {
		return nil, err
	}
	items, total, err := c.svc.GetMerchantSubjectCreditTransactions(ctx, payer, strings.TrimSpace(currency), limit, 0)
	if err != nil {
		return nil, invalidErr(err.Error())
	}
	out := make([]serviceTxn, 0, len(items))
	for _, t := range items {
		out = append(out, serviceTxn{
			ID: t.ID, MerchantSubjectID: t.MerchantSubjectID, Actor: t.Actor, Amount: t.Amount,
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
func (c *localClient) UsageRollup(ctx context.Context, payerTenantID string, from, to time.Time, groupBy string) ([]openrails.UsageRollupRow, error) {
	ctx = c.ensureTenant(ctx)
	// gin binding: tenant_subject_id, from, to, group_by are all required.
	switch {
	case strings.TrimSpace(payerTenantID) == "":
		return nil, bindRequiredErr("MerchantSubjectID")
	case from.UTC().Unix() == 0:
		return nil, bindRequiredErr("From")
	case to.UTC().Unix() == 0:
		return nil, bindRequiredErr("To")
	case strings.TrimSpace(groupBy) == "":
		return nil, bindRequiredErr("GroupBy")
	}
	payer, err := parseMerchantSubject(payerTenantID, "invalid tenant_subject_id")
	if err != nil {
		return nil, err
	}
	rows, err := c.svc.ServiceUsageRollup(ctx, billingservice.ServiceUsageRollupRequest{
		MerchantSubjectID: &payer,
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
		out = append(out, openrails.UsageRollupRow{Key: r.Key, EventCount: r.EventCount, TotalAmount: r.TotalAmount})
	}
	return out, nil
}

// BudgetCheck transcribes handlers.ServiceBudgetCheck (service_admission.go).
func (c *localClient) BudgetCheck(ctx context.Context, tenantSubjectID, actorID string, windows []openrails.BudgetWindowInput, requestedMicros int64) ([]openrails.BudgetWindow, error) {
	ctx = c.ensureTenant(ctx)
	payer, err := parseMerchantSubject(tenantSubjectID, "tenant_subject_id required")
	if err != nil {
		return nil, err
	}
	in := make([]billingservice.BudgetCheckWindowInput, 0, len(windows))
	for _, w := range windows {
		in = append(in, billingservice.BudgetCheckWindowInput{
			Key: w.Key, WindowSeconds: w.WindowSeconds, LimitMicros: w.LimitMicros, Cadence: w.Cadence,
		})
	}
	statuses, err := c.svc.BudgetCheck(ctx, payer, actorID, in, requestedMicros)
	if err != nil {
		return nil, internalErr("budget check failed")
	}
	out := make([]openrails.BudgetWindow, 0, len(statuses))
	for _, w := range statuses {
		out = append(out, budgetWindowFromDTO(w))
	}
	return out, nil
}

// BudgetStatus transcribes handlers.ServiceGetBudget (service_admission.go).
func (c *localClient) BudgetStatus(ctx context.Context, tenantSubjectID, actorID, tier string) ([]openrails.BudgetWindow, error) {
	ctx = c.ensureTenant(ctx)
	payer, err := parseMerchantSubject(tenantSubjectID, "tenant_subject_id required")
	if err != nil {
		return nil, err
	}
	statuses, err := c.svc.BudgetStatus(ctx, payer, actorID, tier)
	if err != nil {
		return nil, internalErr("budget lookup failed")
	}
	out := make([]openrails.BudgetWindow, 0, len(statuses))
	for _, w := range statuses {
		out = append(out, budgetWindowFromDTO(w))
	}
	return out, nil
}

// SetTierPolicy transcribes handlers.ServiceSetTierPolicy
// (service_admission.go).
func (c *localClient) SetTierPolicy(ctx context.Context, tenantSubjectID string, in openrails.TierPolicyInput) error {
	ctx = c.ensureTenant(ctx)
	// An EMPTY tenant_subject_id sets the tenant-wide DEFAULT tier policy (#477,
	// the platform capacity ladder declared once); a non-empty one is a per-subject
	// override.
	var payer identity.MerchantSubjectID
	if strings.TrimSpace(tenantSubjectID) != "" {
		var err error
		payer, err = parseMerchantSubject(tenantSubjectID, "invalid tenant_subject_id")
		if err != nil {
			return err
		}
	}
	pol := billingservice.TierPolicyInput{
		Tier:              in.Tier,
		EntitledResources: in.EntitledResources,
	}
	for _, w := range in.Windows {
		pol.Windows = append(pol.Windows, billingservice.TierWindowInput{
			Unit: w.Unit, WindowSeconds: w.WindowSeconds, Max: w.Max,
		})
	}
	for _, b := range in.BudgetWindows {
		pol.BudgetWindows = append(pol.BudgetWindows, billingservice.TierBudgetWindowInput{
			Key: b.Key, WindowSeconds: b.WindowSeconds, LimitMicros: b.LimitMicros, Cadence: b.Cadence,
		})
	}
	for _, q := range in.QueueLimits {
		pol.QueueLimits = append(pol.QueueLimits, billingservice.TierQueueLimitInput{
			Unit: q.Unit, Max: q.Max,
		})
	}
	if err := c.svc.SetTierPolicy(ctx, payer, pol); err != nil {
		return internalErr("set tier policy failed")
	}
	return nil
}

// SetTierSchedule transcribes handlers.ServiceSetTierSchedule
// (service_admission.go, #476). An empty tenant_subject_id is the tenant-wide
// default schedule (zero payer); a non-empty one is a per-subject override.
func (c *localClient) SetTierSchedule(ctx context.Context, tenantSubjectID string, schedule []openrails.TierScheduleRung) error {
	ctx = c.ensureTenant(ctx)
	var payer identity.MerchantSubjectID
	if strings.TrimSpace(tenantSubjectID) != "" {
		var err error
		payer, err = parseMerchantSubject(tenantSubjectID, "invalid tenant_subject_id")
		if err != nil {
			return err
		}
	}
	rungs := make([]billingservice.TierScheduleRung, 0, len(schedule))
	for _, r := range schedule {
		rungs = append(rungs, billingservice.TierScheduleRung{
			Tier: r.Tier, MinCumulativePaidMicros: r.MinCumulativePaidMicros,
		})
	}
	if err := c.svc.SetTierSchedule(ctx, payer, rungs); err != nil {
		return internalErr("set tier schedule failed")
	}
	return nil
}

// GetTier transcribes handlers.ServiceGetTier (service_admission.go, #477).
func (c *localClient) GetTier(ctx context.Context, tenantSubjectID string) (string, error) {
	ctx = c.ensureTenant(ctx)
	payer, err := parseMerchantSubject(tenantSubjectID, "invalid tenant_subject_id")
	if err != nil {
		return "", err
	}
	tier, terr := c.svc.GetTier(ctx, payer)
	if terr != nil {
		return "", internalErr("tier lookup failed")
	}
	return tier, nil
}

func budgetScopeWindowInputs(ws []openrails.BudgetScopeWindow) []billingservice.BudgetScopeWindowInput {
	out := make([]billingservice.BudgetScopeWindowInput, 0, len(ws))
	for _, w := range ws {
		out = append(out, billingservice.BudgetScopeWindowInput{
			Key: w.Key, WindowSeconds: w.WindowSeconds, LimitMicros: w.LimitMicros, Cadence: w.Cadence,
		})
	}
	return out
}

// SetSubjectBudgetPolicy transcribes handlers.ServiceSetSubjectBudgetPolicy
// (service_admission.go, #473). role_id maps onto the facade's ScopeKey.
func (c *localClient) SetSubjectBudgetPolicy(ctx context.Context, tenantSubjectID string, in openrails.SubjectBudgetPolicyInput) error {
	ctx = c.ensureTenant(ctx)
	payer, err := parseMerchantSubject(tenantSubjectID, "tenant_subject_id required")
	if err != nil {
		return err
	}
	if err := c.svc.SetSubjectBudgetPolicy(ctx, payer, billingservice.BudgetScopePolicyInput{
		Scope:    in.Scope,
		ScopeKey: in.RoleID,
		Windows:  budgetScopeWindowInputs(in.Windows),
	}); err != nil {
		return internalErr("set subject budget policy failed")
	}
	return nil
}

// SetPlatformBudgetPolicy transcribes handlers.ServiceSetPlatformBudgetPolicy
// (service_admission.go, #473). The platform-admin route gate is not transcribed
// — embedded hosts are the principal (the direct call is operator-trusted).
func (c *localClient) SetPlatformBudgetPolicy(ctx context.Context, tenantSubjectID string, in openrails.PlatformBudgetPolicyInput) error {
	ctx = c.ensureTenant(ctx)
	payer, err := parseMerchantSubject(tenantSubjectID, "tenant_subject_id required")
	if err != nil {
		return err
	}
	if err := c.svc.SetPlatformBudgetPolicy(ctx, payer, billingservice.BudgetScopePolicyInput{
		Scope:   in.Scope,
		Windows: budgetScopeWindowInputs(in.Windows),
	}); err != nil {
		return internalErr("set platform budget policy failed")
	}
	return nil
}

// SubjectBudgetPolicies transcribes handlers.ServiceGetSubjectBudgetPolicies
// (service_admission.go, #473): subject-owned policies only; ScopeKey maps back
// onto role_id.
func (c *localClient) SubjectBudgetPolicies(ctx context.Context, tenantSubjectID string) ([]openrails.SubjectBudgetPolicy, error) {
	ctx = c.ensureTenant(ctx)
	payer, err := parseMerchantSubject(tenantSubjectID, "tenant_subject_id required")
	if err != nil {
		return nil, err
	}
	policies, err := c.svc.SubjectBudgetPolicies(ctx, payer)
	if err != nil {
		return nil, internalErr("subject budget policies lookup failed")
	}
	out := make([]openrails.SubjectBudgetPolicy, 0, len(policies))
	for _, p := range policies {
		ws := make([]openrails.BudgetScopeWindow, 0, len(p.Windows))
		for _, w := range p.Windows {
			ws = append(ws, openrails.BudgetScopeWindow{Key: w.Key, WindowSeconds: w.WindowSeconds, LimitMicros: w.LimitMicros, Cadence: w.Cadence})
		}
		out = append(out, openrails.SubjectBudgetPolicy{Scope: p.Scope, RoleID: p.ScopeKey, Windows: ws})
	}
	return out, nil
}

func wireEntitlementRecords(recs []billingservice.EntitlementRecord) []openrails.EntitlementRecord {
	out := make([]openrails.EntitlementRecord, 0, len(recs))
	for _, e := range recs {
		rec := openrails.EntitlementRecord{
			ID:              e.ID.String(),
			MerchantSubjectID: e.MerchantSubjectID.String(),
			Entitlement:     e.Entitlement,
			StartAt:         e.StartAt,
			EndAt:           e.EndAt,
			SourceType:      e.SourceType,
			RevokedAt:       e.RevokedAt,
			RevokeReason:    e.RevokeReason,
			CreatedAt:       e.CreatedAt,
			UpdatedAt:       e.UpdatedAt,
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
func (c *localClient) ResourceRevenueDaily(ctx context.Context, resource string, fromUnix, toUnix int64) (*openrails.ResourceRevenueResponse, error) {
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
	rows, err := c.svc.ResourceRevenueDaily(ctx, resource, time.Unix(fromUnix, 0).UTC(), time.Unix(toUnix, 0).UTC())
	if err != nil {
		return nil, invalidErr(err.Error())
	}
	out := &openrails.ResourceRevenueResponse{Daily: make([]openrails.ResourceRevenueDailyRow, 0, len(rows))}
	for _, r := range rows {
		out.RevenueMicros += r.AmountMicros
		out.Daily = append(out.Daily, openrails.ResourceRevenueDailyRow{Date: r.Date, AmountMicros: r.AmountMicros})
	}
	return out, nil
}

// maxWindowBatchItems transcribes the handler bound on one settle/admit batch
// (service_credit_window.go).
const maxWindowBatchItems = 1000

func windowFromDTO(w *billingservice.CreditWindowDTO) *openrails.CreditWindow {
	return &openrails.CreditWindow{
		WindowID:      w.WindowID,
		PayerTenantID: w.MerchantSubjectID,
		HeldMicros:    w.HeldAmount,
		SettledMicros: w.SettledAmount,
		Status:        w.Status,
		ExpiresAt:     w.ExpiresAt,
	}
}

// mapWindowErr transcribes handlers.writeWindowError (service_credit_window.go).
func mapWindowErr(err error, fallback string) error {
	switch {
	case errors.Is(err, billingservice.ErrWindowNotFound):
		return openrails.NewStatusError(http.StatusNotFound, "", "window_not_found")
	case errors.Is(err, billingservice.ErrWindowNotOpen):
		return openrails.NewStatusError(http.StatusConflict, "", "window_not_open")
	case errors.Is(err, billingservice.ErrInsufficientCredits):
		return openrails.NewStatusError(http.StatusPaymentRequired, "", "insufficient_credits")
	default:
		return internalErr(fallback)
	}
}

// OpenWindow transcribes handlers.ServiceOpenCreditWindow
// (service_credit_window.go, #335).
func (c *localClient) OpenWindow(ctx context.Context, req openrails.OpenWindowRequest) (*openrails.CreditWindow, error) {
	ctx = c.ensureTenant(ctx)
	if req.Currency == "" {
		req.Currency = c.currency
	}
	// gin binding: amount is `binding:"required"`. Currency is optional (#476).
	if req.AmountMicros == 0 {
		return nil, bindRequiredErr("Amount")
	}
	payer, err := parseMerchantSubject(req.PayerTenantID, "tenant_subject_id required")
	if err != nil {
		return nil, err
	}
	w, serr := c.svc.OpenWindow(ctx, billingservice.OpenWindowRequest{
		MerchantSubjectID: payer,
		Actor:           req.Actor,
		Currency:        req.Currency,
		Amount:          req.AmountMicros,
		TTL:             time.Duration(req.TTLSeconds) * time.Second,
	})
	switch {
	case errors.Is(serr, billingservice.ErrInsufficientCredits):
		return nil, openrails.NewStatusError(http.StatusPaymentRequired, "", "insufficient_credits")
	case errors.Is(serr, billingservice.ErrCreditTypeInactive):
		return nil, invalidErr("credit_type_inactive")
	case serr != nil:
		return nil, internalErr("open window failed")
	}
	return windowFromDTO(w), nil
}

// SettleWindowItems transcribes handlers.ServiceSettleCreditWindows
// (service_credit_window.go). The handler's unparseable-window-id skip is
// unreachable through typed items (WindowID is a uuid.UUID; a nil one gets the
// engine's own per-item invalid_item result, same as the wire).
func (c *localClient) SettleWindowItems(ctx context.Context, items []openrails.WindowSettleItem) ([]openrails.WindowSettleResult, error) {
	ctx = c.ensureTenant(ctx)
	if len(items) == 0 {
		return nil, invalidErr("items required")
	}
	if len(items) > maxWindowBatchItems {
		return nil, invalidErr("too many items")
	}
	in := make([]billingservice.WindowSettleItemInput, 0, len(items))
	for _, it := range items {
		item := billingservice.WindowSettleItemInput{
			WindowID:  it.WindowID,
			RequestID: it.RequestID,
			Amount:    it.AmountMicros,
			Actor:     it.Actor,
		}
		if it.Usage != nil {
			item.EventType = it.Usage.EventType
			item.Resource = it.Usage.Resource
			item.Metadata = it.Usage.Metadata
		}
		in = append(in, item)
	}
	settled, err := c.svc.SettleWindowItems(ctx, in)
	if err != nil {
		return nil, internalErr("settle failed")
	}
	out := make([]openrails.WindowSettleResult, 0, len(settled))
	for _, res := range settled {
		out = append(out, openrails.WindowSettleResult{
			WindowID:      res.WindowID,
			RequestID:     res.RequestID,
			OK:            res.OK,
			Replayed:      res.Replayed,
			Error:         res.ErrorCode,
			TransactionID: res.TransactionID,
		})
	}
	return out, nil
}

// RefillWindow transcribes handlers.ServiceRefillCreditWindow
// (service_credit_window.go).
func (c *localClient) RefillWindow(ctx context.Context, windowID uuid.UUID, amountMicros, ttlSeconds int64) (*openrails.CreditWindow, error) {
	ctx = c.ensureTenant(ctx)
	if amountMicros <= 0 && ttlSeconds <= 0 {
		return nil, invalidErr("amount or ttl_seconds required")
	}
	w, err := c.svc.RefillWindow(ctx, windowID, amountMicros, time.Duration(ttlSeconds)*time.Second)
	if err != nil {
		return nil, mapWindowErr(err, "refill failed")
	}
	return windowFromDTO(w), nil
}

// CloseWindow transcribes handlers.ServiceCloseCreditWindow
// (service_credit_window.go).
func (c *localClient) CloseWindow(ctx context.Context, windowID uuid.UUID) (*openrails.CreditWindow, error) {
	ctx = c.ensureTenant(ctx)
	w, err := c.svc.CloseWindow(ctx, windowID)
	if err != nil {
		return nil, mapWindowErr(err, "close failed")
	}
	return windowFromDTO(w), nil
}

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
	if len(items) > maxWindowBatchItems {
		return nil, invalidErr("too many items")
	}
	out := make([]openrails.AdmitBatchVerdict, len(items))
	for i, item := range items {
		if item.EstimateMicros < 0 {
			out[i] = openrails.AdmitBatchVerdict{Status: http.StatusBadRequest, Error: "estimate_micros must be >= 0"}
			continue
		}
		payer, perr := parseMerchantSubject(item.PayerTenantID, "tenant_subject_id required")
		if perr != nil {
			out[i] = openrails.AdmitBatchVerdict{Status: http.StatusBadRequest, Error: "tenant_subject_id required"}
			continue
		}
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
	case "throughput":
		return http.StatusTooManyRequests
	case "money":
		return http.StatusPaymentRequired
	default: // suspended, blocked, unverified, endpoint
		return http.StatusForbidden
	}
}

func payerString(p *openrails.PayerTenantID) string {
	if p == nil || p.IsZero() {
		return ""
	}
	return p.String()
}
