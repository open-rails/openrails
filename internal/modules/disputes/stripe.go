package disputes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/disputerules"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// StripeDispute is a Stripe dispute object, as a webhook or a pull reads it.
type StripeDispute struct {
	ID                   string `json:"id"`
	Charge               string `json:"charge"`
	PaymentIntent        string `json:"payment_intent"`
	Amount               int64  `json:"amount"`
	Currency             string `json:"currency"`
	Status               string `json:"status"`
	Reason               string `json:"reason"`
	Created              int64  `json:"created"`
	PaymentMethodDetails struct {
		Card struct {
			Brand             string `json:"brand"`
			CaseType          string `json:"case_type"`
			NetworkReasonCode string `json:"network_reason_code"`
		} `json:"card"`
	} `json:"payment_method_details"`
	EvidenceDetails struct {
		DueBy int64 `json:"due_by"`
	} `json:"evidence_details"`
}

// Kind is what the dispute is, by its status and case type.
func (d StripeDispute) Kind() billing.DisputeKind {
	return disputerules.StripeKind(d.Status, d.PaymentMethodDetails.Card.CaseType)
}

// Report is what the dispute reports of its case on psp; payment is the
// disputed charge, nil when OpenRails has none.
func (d StripeDispute) Report(psp uuid.UUID, payment *models.Payment, at time.Time) (Report, error) {
	id := strings.TrimSpace(d.ID)
	if id == "" || d.Amount <= 0 {
		return Report{}, errors.New("stripe dispute missing id or amount")
	}
	currency := strings.ToUpper(strings.TrimSpace(d.Currency))
	amount, err := moneyutil.RailMinorToNative(currency, moneyutil.Cents(d.Amount))
	if err != nil {
		return Report{}, fmt.Errorf("stripe dispute %s amount: %w", id, err)
	}
	card := d.PaymentMethodDetails.Card
	r := Report{PSPID: psp, Rail: models.RailStripe, ProviderRef: id, Payment: payment, Kind: d.Kind(),
		Status: disputerules.StripeStatus(d.Status), ReasonCode: card.NetworkReasonCode, ProviderReason: d.Reason,
		Amount: amount, Currency: currency, At: at}
	if card.Brand != "" {
		r.Network = disputerules.Network(card.Brand)
	}
	if d.Created > 0 {
		r.DisputedAt = time.Unix(d.Created, 0).UTC()
	}
	if due := d.EvidenceDetails.DueBy; due > 0 {
		t := time.Unix(due, 0).UTC()
		r.EvidenceDueAt = &t
	}
	return r, nil
}

// StripeCharge is the charge a Stripe object names, by its charge or its
// PaymentIntent; nil when OpenRails has no record of either.
func StripeCharge(ctx context.Context, d *db.DB, charge, intent string) (*models.Payment, error) {
	svc := payments.NewPaymentService(d)
	for _, ref := range []string{strings.TrimSpace(charge), strings.TrimSpace(intent)} {
		if ref == "" {
			continue
		}
		payment, err := svc.GetByPSPTransactionID(ctx, models.RailStripe, ref)
		if err == nil {
			return payment, nil
		}
		if !db.IsNotFound(err) {
			return nil, fmt.Errorf("lookup stripe charge %s: %w", ref, err)
		}
	}
	return nil, nil
}

// RecordPulledDisputes records every case a pull read on psp, whatever its
// kind, as its webhook would have: a chargeback's reversal stays the pull's
// proposal to apply.
func (s *Service) RecordPulledDisputes(ctx context.Context, psp uuid.UUID, provider reconcile.Provider, cases []json.RawMessage) error {
	if s == nil || provider != reconcile.ProviderStripe {
		return nil
	}
	var errs []error
	for _, raw := range cases {
		var d StripeDispute
		if err := json.Unmarshal(raw, &d); err != nil {
			errs = append(errs, fmt.Errorf("parse pulled stripe dispute: %w", err))
			continue
		}
		payment, err := StripeCharge(ctx, s.DB, d.Charge, d.PaymentIntent)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		report, err := d.Report(psp, payment, s.now())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := s.Record(ctx, s.DB, report); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RaiseUnknownCharge records that a dispute or fraud report names a charge
// OpenRails has no record of: staff find where it was taken.
func (s *Service) RaiseUnknownCharge(ctx context.Context, d *db.DB, psp uuid.UUID, ref, charge string) error {
	if _, err := merchant.Require(ctx); err != nil {
		return err
	}
	action := fmt.Sprintf("The provider reported %s on charge %s, which OpenRails has no record of. Find where it was taken; nothing was refunded, ended or blocked.", ref, charge)
	_, err := (&reconcile.PGStore{DB: d}).RaiseFinding(ctx, reconcile.RaisedFinding{Type: FindingUnknownCharge, SubjectKey: ref, PSPID: &psp,
		Severity: reconcile.SeverityHigh, RecommendedAction: action, Evidence: map[string]any{"provider_ref": ref, "charge": charge, "psp_id": billing.PSPID(psp).String()}})
	if err != nil {
		return fmt.Errorf("raise unknown charge %s: %w", ref, err)
	}
	return nil
}
