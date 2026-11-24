// Package disputes records each provider dispute and issuer fraud report,
// keeps a charged-back card from being charged again, and evaluates the card
// networks' monitoring programs per PSP, raising findings that alert.
package disputes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/disputerules"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// Service records disputes and evaluates the monitoring programs.
type Service struct {
	DB    *db.DB
	Clock clockwork.Clock
	// Notifier alerts the operator of a finding; nil leaves it in the console.
	Notifier reconcile.FindingNotifier
	// Limits are each PSP's declared dispute_ratio_limit, in basis points;
	// nil declares none.
	Limits func(ctx context.Context, psp uuid.UUID) (int64, bool)
	Log    *slog.Logger
}

// New is a Service on database.
func New(database *db.DB, clock clockwork.Clock) *Service {
	return &Service{DB: database, Clock: timeutil.FirstClock(clock), Log: slog.New(slog.DiscardHandler)}
}

func (s *Service) now() time.Time { return s.Clock.Now().UTC().Truncate(time.Microsecond) }

// Report is what a rail reports of one dispute case.
type Report struct {
	PSPID       uuid.UUID
	Rail        models.Rail
	ProviderRef string
	// Payment is the disputed charge; nil when the report matched none.
	Payment *models.Payment
	Kind    billing.DisputeKind
	Status  billing.DisputeStatus
	// Network is the card network; "" takes the charge's card.
	Network        billing.CardNetwork
	ReasonCode     string
	ProviderReason string
	// Amount and Currency are what is disputed; zero and "" when the report
	// named no currency it could be matched in.
	Amount   int64
	Currency string
	// DisputedAt is when the case opened at the provider; At when this report
	// says what it says. Zero is now.
	DisputedAt    time.Time
	At            time.Time
	EvidenceDueAt *time.Time
}

// Case is a dispute as recorded.
type Case struct {
	gen.BillingDispute
	// BecameChargeback is true when this report made the case a chargeback:
	// its first report as one, or the issuer's escalation of an inquiry.
	BecameChargeback bool
}

// ID is the case's typed id.
func (c Case) ID() billing.DisputeID { return billing.DisputeID(c.BillingDispute.ID) }

// Record writes what r reports, in d's transaction (one of its own when d
// holds none). A won case stays won; a chargeback stays one. A case that
// becomes a chargeback queues its PSP's monitor and its customer's review.
func (s *Service) Record(ctx context.Context, d *db.DB, r Report) (Case, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return Case{}, err
	}
	if r.PSPID == uuid.Nil || strings.TrimSpace(r.ProviderRef) == "" {
		return Case{}, errors.New("dispute: PSP and provider reference are required")
	}
	now := s.now()
	at, disputedAt := orNow(r.At, now), orNow(r.DisputedAt, now)
	network := r.Network
	if network == "" && r.Payment != nil {
		network = disputerules.Network(models.DerefStr(r.Payment.CardBrand))
	}
	if network == "" {
		network = billing.NetworkUnknown
	}
	reason := disputerules.Classify(network, r.ReasonCode, r.ProviderReason)
	// Only a chargeback is won, lost or unreported; any other case ends closed.
	if r.Kind != billing.DisputeChargeback && (r.Status == billing.DisputeWon || r.Status == billing.DisputeLost || r.Status == billing.DisputeUnreported) {
		r.Status = billing.DisputeClosed
	}
	if r.Kind == billing.DisputePrevented {
		r.Status = billing.DisputeClosed
	}
	var out Case
	err = d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txDB := d.NewWithPgxTx(tx)
		q := txDB.Gen(ctx)
		params := gen.InsertDisputeParams{
			MerchantID: mid.UUID(), PspID: r.PSPID, Rail: string(r.Rail), ProviderRef: strings.TrimSpace(r.ProviderRef),
			Kind: string(r.Kind), Network: string(network), Reason: string(reason),
			ReasonCode: nonEmpty(r.ReasonCode), ProviderReason: nonEmpty(r.ProviderReason),
			Status: string(r.Status), DisputedAt: disputedAt, EvidenceDueAt: r.EvidenceDueAt,
		}
		if r.Payment != nil {
			params.PaymentID, params.CustomerID = &r.Payment.ID, &r.Payment.CustomerID
		}
		if r.Amount > 0 && r.Currency != "" {
			params.Amount, params.Currency = &r.Amount, &r.Currency
		}
		// A case reported as a chargeback is one from when it opened.
		if r.Kind == billing.DisputeChargeback {
			params.ChargebackAt = &disputedAt
		}
		if closes(r.Status) {
			params.ClosedAt = &at
		}
		row, err := q.InsertDispute(ctx, params)
		switch {
		case err == nil:
			out = Case{BillingDispute: row, BecameChargeback: r.Kind == billing.DisputeChargeback}
		case errors.Is(err, pgx.ErrNoRows):
			existing, err := q.GetDisputeForUpdate(ctx, gen.GetDisputeForUpdateParams{MerchantID: mid.UUID(), PspID: r.PSPID, ProviderRef: params.ProviderRef})
			if err != nil {
				return fmt.Errorf("lock dispute %s: %w", params.ProviderRef, err)
			}
			if out, err = s.update(ctx, q, existing, r, network, reason, at); err != nil {
				return err
			}
		default:
			return fmt.Errorf("record dispute %s: %w", params.ProviderRef, err)
		}
		if !out.BecameChargeback {
			return nil
		}
		if out.CustomerID != nil {
			if err := raiseCustomerReview(ctx, txDB, *out.CustomerID, out.BillingDispute); err != nil {
				return err
			}
		}
		return QueueMonitor(ctx, txDB, mid, r.PSPID)
	})
	return out, err
}

func (s *Service) update(ctx context.Context, q *gen.Queries, existing gen.BillingDispute, r Report, network billing.CardNetwork, reason billing.DisputeReason, at time.Time) (Case, error) {
	next := gen.UpdateDisputeParams{
		MerchantID: existing.MerchantID, ID: existing.ID, Kind: existing.Kind, Network: existing.Network, Reason: existing.Reason,
		ReasonCode: existing.ReasonCode, ProviderReason: existing.ProviderReason, Amount: existing.Amount, Currency: existing.Currency,
		Status: existing.Status, ChargebackAt: existing.ChargebackAt, EvidenceDueAt: existing.EvidenceDueAt, ClosedAt: existing.ClosedAt,
		ReversalPaymentID: existing.ReversalPaymentID, RecoveryPaymentID: existing.RecoveryPaymentID,
	}
	became := false
	if existing.Kind != string(billing.DisputeChargeback) && r.Kind != "" {
		next.Kind = string(r.Kind)
		if r.Kind == billing.DisputeChargeback {
			next.ChargebackAt, became = &at, true
		}
	}
	if network != billing.NetworkUnknown {
		next.Network = string(network)
	}
	if reason != billing.DisputeUnknownReason {
		next.Reason = string(reason)
	}
	if v := nonEmpty(r.ReasonCode); v != nil {
		next.ReasonCode = v
	}
	if v := nonEmpty(r.ProviderReason); v != nil {
		next.ProviderReason = v
	}
	if r.Amount > 0 && r.Currency != "" {
		next.Amount, next.Currency = &r.Amount, &r.Currency
	}
	if r.EvidenceDueAt != nil {
		next.EvidenceDueAt = r.EvidenceDueAt
	}
	// Won is final; anything else follows the latest report.
	if existing.Status != string(billing.DisputeWon) && r.Status != "" {
		if next.Kind == string(billing.DisputeChargeback) || (r.Status != billing.DisputeWon && r.Status != billing.DisputeLost && r.Status != billing.DisputeUnreported) {
			next.Status = string(r.Status)
		}
	}
	switch {
	case !closes(billing.DisputeStatus(next.Status)):
		next.ClosedAt = nil
	case next.ClosedAt == nil:
		next.ClosedAt = &at
	}
	row, err := q.UpdateDispute(ctx, next)
	if err != nil {
		return Case{}, fmt.Errorf("update dispute %s: %w", existing.ProviderRef, err)
	}
	return Case{BillingDispute: row, BecameChargeback: became}, nil
}

// Link names the payments that took the case's money back (reversal) and
// returned it (recovery); nil leaves each as it is.
func (s *Service) Link(ctx context.Context, d *db.DB, c Case, reversal, recovery *uuid.UUID) error {
	if reversal == nil && recovery == nil {
		return nil
	}
	q := d.Gen(ctx)
	current, err := q.GetDispute(ctx, gen.GetDisputeParams{MerchantID: c.MerchantID, ID: c.BillingDispute.ID})
	if err != nil {
		return fmt.Errorf("load dispute %s: %w", c.ProviderRef, err)
	}
	next := gen.UpdateDisputeParams{
		MerchantID: current.MerchantID, ID: current.ID, Kind: current.Kind, Network: current.Network, Reason: current.Reason,
		ReasonCode: current.ReasonCode, ProviderReason: current.ProviderReason, Amount: current.Amount, Currency: current.Currency,
		Status: current.Status, ChargebackAt: current.ChargebackAt, EvidenceDueAt: current.EvidenceDueAt, ClosedAt: current.ClosedAt,
		ReversalPaymentID: current.ReversalPaymentID, RecoveryPaymentID: current.RecoveryPaymentID,
	}
	if reversal != nil {
		next.ReversalPaymentID = reversal
	}
	if recovery != nil {
		next.RecoveryPaymentID = recovery
	}
	if _, err := q.UpdateDispute(ctx, next); err != nil {
		return fmt.Errorf("link dispute %s: %w", c.ProviderRef, err)
	}
	return nil
}

func closes(s billing.DisputeStatus) bool {
	return s == billing.DisputeWon || s == billing.DisputeLost || s == billing.DisputeClosed
}

func orNow(t, now time.Time) time.Time {
	if t.IsZero() {
		return now
	}
	return t.UTC().Truncate(time.Microsecond)
}

func nonEmpty(s string) *string {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	return &s
}
