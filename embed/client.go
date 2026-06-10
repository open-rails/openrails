package embed

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/pkg/identity"
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
// The service-token tenant-subject scope gate (requireServiceTenantSubjectScope)
// is intentionally NOT transcribed: in embedded mode the host IS the principal
// and is trusted for its own tenant, exactly like a tenant-wide service token.
type localClient struct {
	svc *billingservice.Service
	// creditType is the default credit_type applied to Admit/Authorize/Balance
	// when the request leaves it empty — the client-side counterpart of
	// openrails.WithCreditType on the remote.
	creditType string
}

// ClientOption configures Runtime.Client.
type ClientOption func(*localClient)

// WithCreditType mirrors openrails.WithCreditType for the embedded client.
func WithCreditType(creditType string) ClientOption {
	return func(c *localClient) { c.creditType = strings.TrimSpace(creditType) }
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

// parseTenantSubject transcribes handlers.parseServiceTenantSubjectID +
// the per-handler nil/err handling. invalidMsg is the message the handler uses
// for an unparseable id; an empty/missing id is always "tenant_subject_id
// required".
func parseTenantSubject(raw, invalidMsg string) (identity.TenantSubjectID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return identity.TenantSubjectID{}, invalidErr("tenant_subject_id required")
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return identity.TenantSubjectID{}, invalidErr(invalidMsg)
	}
	return identity.TenantSubjectID(id), nil
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
		TenantSubjectID: t.TenantSubjectID,
		Actor:           t.Actor,
		CreditTypeID:    t.CreditTypeID,
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

// normalizeCreditType mirrors the root package's client-side defaulting for
// GetCreditAccount/SetCreditAccountSettings/ListCreditTransactions: an empty
// credit type means the built-in "usd_micro" micro-dollar ledger.
func normalizeCreditType(creditType string) string {
	creditType = strings.TrimSpace(creditType)
	if creditType == "" {
		return "usd_micro"
	}
	return creditType
}

// --- Client implementation -------------------------------------------------

// Admit transcribes handlers.ServiceAdmit + admitInputFromRequest
// (service_admission.go). All verdict outcomes — allowed, throughput (429),
// money (402), gated (403) — return (resp, nil), like the remote client.
func (c *localClient) Admit(ctx context.Context, req openrails.AdmitRequest) (*openrails.AdmitResponse, error) {
	if req.CreditType == "" {
		req.CreditType = c.creditType
	}
	if req.EstimateMicros < 0 {
		return nil, invalidErr("estimate_micros must be >= 0")
	}
	payer, err := parseTenantSubject(req.PayerTenantID, "tenant_subject_id required")
	if err != nil {
		return nil, err
	}

	in := billingservice.AdmitInput{
		TenantSubjectID: payer,
		Actor:           strings.TrimSpace(req.Actor),
		Tier:            req.Tier,
		Resource:        req.Resource,
		Amounts:         req.Amounts,
		CreditType:      req.CreditType,
		EstimateMicros:  req.EstimateMicros,
		Source:          req.Source,
		SourceID:        req.RequestID,
	}
	if req.ExpiresAt != nil {
		in.ExpiresAtUnix = *req.ExpiresAt
	}
	for _, w := range req.TenantThroughput {
		in.TenantThroughput = append(in.TenantThroughput, billingservice.AdmitThroughputWindow{
			Unit: w.Unit, WindowSeconds: w.WindowSeconds, Max: w.Max,
		})
	}

	res, err := c.svc.Admit(ctx, in)
	if err != nil {
		return nil, internalErr("admission check failed")
	}
	out := &openrails.AdmitResponse{
		Allowed:             res.Allowed,
		BlockedBy:           res.BlockedBy,
		BlockedUnit:         res.BlockedUnit,
		DenyCode:            res.DenyCode,
		RetryAfterSeconds:   res.RetryAfterSeconds,
		ReservationID:       res.ReservationID,
		BudgetReservationID: res.BudgetReservationID,
	}
	for _, w := range res.Windows {
		out.Windows = append(out.Windows, openrails.AdmitWindow{
			Unit: w.Unit, Limit: w.Limit, Remaining: w.Remaining, ResetAfterSeconds: w.ResetAfterSeconds,
		})
	}
	for _, w := range res.BudgetWindows {
		out.BudgetWindows = append(out.BudgetWindows, budgetWindowFromDTO(w))
	}
	return out, nil
}

// Authorize transcribes handlers.ServiceAuthorizeCredits (service_credits.go).
func (c *localClient) Authorize(ctx context.Context, req openrails.AuthorizeRequest) (*openrails.AuthorizeResponse, error) {
	if req.CreditType == "" {
		req.CreditType = c.creditType
	}
	// gin binding: credit_type + request_id are `binding:"required"`.
	if strings.TrimSpace(req.CreditType) == "" {
		return nil, bindRequiredErr("CreditType")
	}
	if strings.TrimSpace(req.RequestID) == "" {
		return nil, bindRequiredErr("RequestID")
	}
	if req.EstimateMicros < 0 {
		return nil, invalidErr("estimate_micros must be >= 0")
	}
	payer, err := parseTenantSubject(req.PayerTenantID, "invalid tenant_subject_id")
	if err != nil {
		return nil, err
	}
	var expiresAt time.Time
	if req.ExpiresAt != nil {
		expiresAt = time.Unix(*req.ExpiresAt, 0).UTC()
	}
	out, err := c.svc.AuthorizeAndHold(ctx, billingservice.AuthorizeAndHoldRequest{
		TenantSubjectID: payer,
		Actor:           req.Actor,
		CreditType:      req.CreditType,
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
	// gin binding (in struct field order): actor, credit_type, amount, source,
	// source_id, expires_at are all `binding:"required"`.
	switch {
	case strings.TrimSpace(req.Actor) == "":
		return nil, bindRequiredErr("Actor")
	case strings.TrimSpace(req.CreditType) == "":
		return nil, bindRequiredErr("CreditType")
	case req.Amount == 0:
		return nil, bindRequiredErr("Amount")
	case strings.TrimSpace(req.Source) == "":
		return nil, bindRequiredErr("Source")
	case strings.TrimSpace(req.SourceID) == "":
		return nil, bindRequiredErr("SourceID")
	}
	payer, err := parseTenantSubject(payerString(req.PayerTenantID), "invalid tenant_subject_id")
	if err != nil {
		return nil, err
	}
	hold, err := c.svc.HoldCredits(ctx, billingservice.HoldCreditsRequest{
		TenantSubjectID: &payer,
		Actor:           req.Actor,
		CreditType:      req.CreditType,
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
	if err := c.svc.ReleaseHold(ctx, holdID); err != nil {
		return internalErr("release failed")
	}
	return nil
}

// WithdrawCredits transcribes handlers.ServiceWithdrawCredits
// (service_credits.go).
func (c *localClient) WithdrawCredits(ctx context.Context, req openrails.WithdrawCreditsRequest) (*openrails.CreditTransaction, error) {
	switch {
	case strings.TrimSpace(req.Actor) == "":
		return nil, bindRequiredErr("Actor")
	case strings.TrimSpace(req.CreditType) == "":
		return nil, bindRequiredErr("CreditType")
	case req.Amount == 0:
		return nil, bindRequiredErr("Amount")
	case strings.TrimSpace(req.Source) == "":
		return nil, bindRequiredErr("Source")
	case req.SourceID == nil:
		return nil, bindRequiredErr("SourceID")
	}
	payer, err := parseTenantSubject(payerString(req.PayerTenantID), "invalid tenant_subject_id")
	if err != nil {
		return nil, err
	}
	trx, err := c.svc.WithdrawCredits(ctx, billingservice.WithdrawCreditsRequest{
		TenantSubjectID: &payer,
		Actor:           req.Actor,
		CreditType:      req.CreditType,
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
	switch {
	case strings.TrimSpace(req.Actor) == "":
		return nil, bindRequiredErr("Actor")
	case strings.TrimSpace(req.CreditType) == "":
		return nil, bindRequiredErr("CreditType")
	case req.Amount == 0:
		return nil, bindRequiredErr("Amount")
	case strings.TrimSpace(req.Source) == "":
		return nil, bindRequiredErr("Source")
	case req.SourceID == nil:
		return nil, bindRequiredErr("SourceID")
	}
	payer, err := parseTenantSubject(payerString(req.PayerTenantID), "invalid tenant_subject_id")
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
		TenantSubjectID: &payer,
		Actor:           req.Actor,
		CreditType:      req.CreditType,
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
		return nil, internalErr("deposit failed")
	}
	return transactionFromService(trx), nil
}

// Capture transcribes handlers.ServiceCaptureHold (service_credits.go)
// addressed by reservation id, including the #311 usage-event extension.
func (c *localClient) Capture(ctx context.Context, reservationID string, capturedMicros int64, usage *openrails.CaptureUsage) error {
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
	holdID, err := uuid.Parse(strings.TrimSpace(reservationID))
	if err != nil {
		return invalidErr("invalid hold id")
	}
	return c.ReleaseHold(ctx, holdID)
}

// Balance transcribes handlers.ServiceGetCreditsBalance (service_credits.go):
// the handler defaults an absent credit_type to "api_credits"; the remote
// client sends its WithCreditType default when set, mirrored here.
func (c *localClient) Balance(ctx context.Context, payerTenantID string) (*openrails.BalanceResponse, error) {
	creditType := c.creditType
	if creditType == "" {
		creditType = "api_credits"
	}
	payer, err := parseTenantSubject(payerTenantID, "invalid tenant_subject_id")
	if err != nil {
		return nil, err
	}
	snap, err := c.svc.GetCreditAccount(ctx, payer, creditType)
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
// (service_credits.go) with the remote client's "usd_micro" normalization.
func (c *localClient) GetCreditAccount(ctx context.Context, payerTenantID, creditType string) (*openrails.CreditAccount, error) {
	payer, err := parseTenantSubject(payerTenantID, "invalid tenant_subject_id")
	if err != nil {
		return nil, err
	}
	snap, err := c.svc.GetCreditAccount(ctx, payer, normalizeCreditType(creditType))
	if err != nil {
		return nil, invalidErr(err.Error())
	}
	return &openrails.CreditAccount{
		PayerTenantID:         snap.TenantSubjectID.String(),
		CreditType:            snap.CreditType,
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
func (c *localClient) SetCreditAccountSettings(ctx context.Context, payerTenantID, creditType string, in openrails.AccountSettingsInput) (*openrails.CreditAccount, error) {
	payer, err := parseTenantSubject(payerTenantID, "tenant_subject_id required")
	if err != nil {
		return nil, err
	}
	ct := normalizeCreditType(creditType)
	settings := credits.AccountSettingsInput{
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
	return c.GetCreditAccount(ctx, payerTenantID, creditType)
}

// serviceTxn mirrors the handler's serviceTxnResponse wire shape
// (service_credits.go) for ListCreditTransactions passthrough JSON.
type serviceTxn struct {
	ID              uuid.UUID `json:"id"`
	TenantSubjectID uuid.UUID `json:"tenant_subject_id"`
	Actor           string    `json:"actor"`
	Amount          int64     `json:"amount"`
	TransactionType string    `json:"transaction_type"`
	Status          string    `json:"status"`
	Source          string    `json:"source"`
	CreatedAt       time.Time `json:"created_at"`
}

// ListCreditTransactions transcribes
// handlers.ServiceListTenantSubjectCreditTransactions (service_credits.go),
// producing the same {"transactions":[...],"total":N} JSON the wire carries.
func (c *localClient) ListCreditTransactions(ctx context.Context, payerTenantID, creditType string, limit int) (json.RawMessage, error) {
	payer, err := parseTenantSubject(payerTenantID, "tenant_subject_id required")
	if err != nil {
		return nil, err
	}
	items, total, err := c.svc.GetTenantSubjectCreditTransactions(ctx, payer, normalizeCreditType(creditType), limit, 0)
	if err != nil {
		return nil, invalidErr(err.Error())
	}
	out := make([]serviceTxn, 0, len(items))
	for _, t := range items {
		out = append(out, serviceTxn{
			ID: t.ID, TenantSubjectID: t.TenantSubjectID, Actor: t.Actor, Amount: t.Amount,
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
	// gin binding: tenant_subject_id, from, to, group_by are all required.
	switch {
	case strings.TrimSpace(payerTenantID) == "":
		return nil, bindRequiredErr("TenantSubjectID")
	case from.UTC().Unix() == 0:
		return nil, bindRequiredErr("From")
	case to.UTC().Unix() == 0:
		return nil, bindRequiredErr("To")
	case strings.TrimSpace(groupBy) == "":
		return nil, bindRequiredErr("GroupBy")
	}
	payer, err := parseTenantSubject(payerTenantID, "invalid tenant_subject_id")
	if err != nil {
		return nil, err
	}
	rows, err := c.svc.ServiceUsageRollup(ctx, billingservice.ServiceUsageRollupRequest{
		TenantSubjectID: &payer,
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
	payer, err := parseTenantSubject(tenantSubjectID, "tenant_subject_id required")
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
	payer, err := parseTenantSubject(tenantSubjectID, "tenant_subject_id required")
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
	payer, err := parseTenantSubject(tenantSubjectID, "tenant_subject_id required")
	if err != nil {
		return err
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
	if err := c.svc.SetTierPolicy(ctx, payer, pol); err != nil {
		return internalErr("set tier policy failed")
	}
	return nil
}

// ResourceRevenueDaily transcribes handlers.ServiceResourceRevenue
// (service_credits.go), including the handler-side total summation.
func (c *localClient) ResourceRevenueDaily(ctx context.Context, resource string, fromUnix, toUnix int64) (*openrails.ResourceRevenueResponse, error) {
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

func payerString(p *openrails.PayerTenantID) string {
	if p == nil || p.IsZero() {
		return ""
	}
	return p.String()
}
