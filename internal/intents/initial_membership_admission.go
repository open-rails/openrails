package intents

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/merchant"
)

func (s *Store) enqueueInitialMembership(ctx context.Context, p EnqueueParams) (gen.OpenrailsRailIntent, error) {
	var row gen.OpenrailsRailIntent
	mid, err := merchant.Require(ctx)
	if err != nil || mid.UUID() != p.MerchantID {
		return row, errors.New("initial enrollment merchant does not match context")
	}
	raw, err := json.Marshal(p.Payload)
	if err != nil {
		return row, err
	}
	target := p.PspID
	if target == uuid.Nil {
		target, err = db.RequirePSPID(ctx)
		if err != nil {
			return row, err
		}
	}
	// Read only the requested lock coordinates here. Full canonical validation
	// runs against the real inserted/existing row before this transaction commits.
	var terms subscriptions.InitialMembershipPayload
	if err := json.Unmarshal(raw, &terms); err != nil {
		return row, err
	}
	customer, err := uuid.Parse(terms.Terms.CustomerID.String())
	if err != nil || customer == uuid.Nil || p.PriceID == nil || *p.PriceID != terms.Terms.PriceID || terms.Instrument.PSPID != target {
		return row, errors.New("initial enrollment admission coordinates contradict requested enrollment")
	}
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: p.MerchantID, ID: customer}); err != nil {
			return err
		}
		store := s.withTxDB(d)
		previous, err := store.GetByIdempotencyKey(ctx, p.IdempotencyKey)
		if err == nil {
			accepted, err := subscriptions.DecodeInitialMembershipPayload(previous)
			if err != nil {
				return err
			}
			if accepted.Terms.CustomerID.String() != terms.Terms.CustomerID.String() || accepted.Terms.PriceID != terms.Terms.PriceID || accepted.RequestFingerprint != terms.RequestFingerprint || accepted.Terms.PaymentMethodID != terms.Terms.PaymentMethodID || accepted.Instrument.PSPID != terms.Instrument.PSPID {
				return apperr.Conflictf("checkout key belongs to another accepted enrollment")
			}
			row = previous
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		_, err = d.Gen(ctx).GetConflictingInitialEnrollmentSubscription(ctx, gen.GetConflictingInitialEnrollmentSubscriptionParams{MerchantID: p.MerchantID, CustomerID: customer, ProductID: terms.Terms.ProductID})
		if err == nil {
			return apperr.Conflictf("customer already has a subscription for this product or tier group")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		_, err = d.Gen(ctx).GetConflictingInitialEnrollmentOperation(ctx, gen.GetConflictingInitialEnrollmentOperationParams{MerchantID: p.MerchantID, CustomerID: customer, ProductID: terms.Terms.ProductID})
		if err == nil {
			return apperr.Conflictf("another enrollment of this product or tier group is unresolved")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		method, err := d.Gen(ctx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: p.MerchantID, ID: terms.Terms.PaymentMethodID})
		if err != nil {
			return err
		}
		if method.CustomerID != customer || method.Rail != p.Provider || method.ParkReason != "" {
			return apperr.Conflictf("initial enrollment instrument changed before admission")
		}
		if err := terms.Instrument.Matches(method, charge.AgreementRecurring); err != nil {
			return err
		}
		if terms.Terms.CollectionPolicy == models.CollectionPolicyEngine && terms.HyperSwitch != nil {
			binding, err := charge.FreezeHyperSwitchBinding(ctx, d.Gen(ctx), method, terms.HyperSwitch.APIBaseURL)
			if err != nil {
				return err
			}
			if binding != *terms.HyperSwitch {
				return charge.ErrInstrumentChanged
			}
		}
		row, err = store.enqueue(ctx, p)
		if err != nil {
			return err
		}
		accepted, err := subscriptions.DecodeInitialMembershipPayload(row)
		if err != nil {
			return err
		}
		if accepted.Terms.CustomerID.String() != terms.Terms.CustomerID.String() || accepted.Terms.PriceID != terms.Terms.PriceID || accepted.RequestFingerprint != terms.RequestFingerprint || accepted.Terms.PaymentMethodID != terms.Terms.PaymentMethodID || accepted.Instrument.PSPID != terms.Instrument.PSPID {
			return apperr.Conflictf("checkout key belongs to another accepted enrollment")
		}
		return nil
	})
	return row, err
}
