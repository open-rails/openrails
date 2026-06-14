package money

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/identity"
	log "github.com/sirupsen/logrus"
)

type GrantSubscriptionCreditsParams struct {
	SubscriptionID uuid.UUID
	PeriodEnd      time.Time
	Cadence        models.CreditGrantCadence // once|per_renewal
	Source         string                    // for deposit transaction (e.g., "subscription_initial", "subscription_renewal")
}

// validateCreditGrantSpec validates one credit/currency grant spec (#472). The
// grant key is just a label; a non-empty key scopes per-grant idempotency. Unit
// defaults to usd; it must be a built-in currency OR an active qualified custom
// credit unit (#475). expiry_days==0 means never-expire (only an explicit
// negative is invalid).
func (s *MoneyService) validateCreditGrantSpec(ctx context.Context, grantKey string, spec models.CreditGrantSpec) error {
	if strings.TrimSpace(grantKey) == "" {
		return fmt.Errorf("grant key is empty")
	}
	if spec.Amount <= 0 {
		return fmt.Errorf("invalid credits_spec: %s amount must be > 0", grantKey)
	}
	if err := s.validateUnit(ctx, spec.UnitOrDefault()); err != nil {
		return fmt.Errorf("invalid credits_spec: %s %w", grantKey, err)
	}
	if spec.ExpiryDays != nil && *spec.ExpiryDays < 0 {
		return fmt.Errorf("invalid credits_spec: %s expiry_days must be >= 0", grantKey)
	}
	if spec.ExpiresDays != nil && *spec.ExpiresDays < 0 {
		return fmt.Errorf("invalid credits_spec: %s expires_days must be >= 0", grantKey)
	}
	cadence := spec.Cadence
	if cadence == "" {
		cadence = models.CreditGrantCadenceOnce
	}
	if cadence != models.CreditGrantCadenceOnce && cadence != models.CreditGrantCadencePerRenewal {
		return fmt.Errorf("invalid credits_spec: %s cadence must be 'once' or 'per_renewal'", grantKey)
	}
	return nil
}

// grantExpiry resolves a grant's deposit expiry: now + EffectiveExpiryDays, or nil
// (never) when the resolved value is 0 (#472).
func grantExpiry(now time.Time, spec models.CreditGrantSpec) *time.Time {
	days := spec.EffectiveExpiryDays()
	if days <= 0 {
		return nil
	}
	t := now.Add(time.Duration(days) * 24 * time.Hour)
	return &t
}

// GrantSubscriptionCredits grants the promo MONEY (µ$) defined in
// product.credits_spec for a subscription event. Each spec entry is a money
// deposit (the spec key is a label, not a credit_type — #472). Idempotent per
// (subscription_id, grant key, period_end) via a deterministic deposit SourceID.
func (s *MoneyService) GrantSubscriptionCredits(ctx context.Context, params GrantSubscriptionCreditsParams) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	if params.SubscriptionID == uuid.Nil {
		return fmt.Errorf("subscription_id required")
	}
	if params.PeriodEnd.IsZero() {
		return fmt.Errorf("period_end required")
	}
	if strings.TrimSpace(string(params.Cadence)) == "" {
		return fmt.Errorf("cadence required")
	}
	if strings.TrimSpace(params.Source) == "" {
		return fmt.Errorf("source required")
	}

	return s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		now := s.now()

		sub, err := q.GetSubscriptionByID(ctx, params.SubscriptionID)
		if err != nil {
			return err
		}

		var creditsSpec models.CreditsSpec
		if err := fromJSONBC(sub.CreditsSpecSnapshot, &creditsSpec, "subscriptions.credits_spec_snapshot"); err != nil {
			return err
		}
		if len(creditsSpec) == 0 {
			prod, perr := q.GetProductByID(ctx, sub.ProductID)
			if perr != nil {
				return perr
			}
			if err := fromJSONBC(prod.CreditsSpec, &creditsSpec, "products.credits_spec"); err != nil {
				return err
			}
		}

		if len(creditsSpec) == 0 {
			return nil
		}

		for label, spec := range creditsSpec {
			label = strings.TrimSpace(label)
			if err := s.validateCreditGrantSpec(ctx, label, spec); err != nil {
				return err
			}

			cadence := spec.Cadence
			if cadence == "" {
				cadence = models.CreditGrantCadenceOnce
			}
			if cadence != params.Cadence {
				continue
			}

			grantKey := fmt.Sprintf("openrails:sub_credit_grant:%s:%s:%s", cadence, sub.ID, label)
			if cadence == models.CreditGrantCadencePerRenewal {
				grantKey = fmt.Sprintf("%s:%s", grantKey, params.PeriodEnd.UTC().Format(time.RFC3339Nano))
			}
			grantID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(grantKey))

			if _, err := s.depositTx(ctx, q, DepositParams{
				Actor:     sub.TenantSubjectID.String(),
				Currency:  spec.UnitOrDefault(),
				Amount:    spec.Amount,
				Source:    strings.TrimSpace(params.Source),
				SourceID:  &grantID,
				ExpiresAt: grantExpiry(now, spec),
			}); err != nil {
				return err
			}

			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": sub.ID,
				"period_end":      params.PeriodEnd.UTC(),
				"grant_label":     label,
				"unit":            spec.UnitOrDefault(),
				"amount":          spec.Amount,
				"expiry_days":     spec.EffectiveExpiryDays(),
				"cadence":         cadence,
				"grant_id":        grantID,
			}).Info("subscription credit grant applied")
		}

		return nil
	})
}

// GrantPurchaseCreditsParams grants a one-off purchase's credit/currency balances
// (#472) — the other half of "what you get" alongside entitlements. Payer is the
// payment owner's tenant subject; PaymentID scopes idempotency per (payment, label).
type GrantPurchaseCreditsParams struct {
	Payer     identity.TenantSubjectID
	PaymentID uuid.UUID
	Spec      models.CreditsSpec
	Source    string // deposit source label (e.g., "purchase")
}

// GrantPurchaseCredits deposits each credits_spec entry of a completed one-off
// purchase as an expiring balance. Idempotent per (payment, grant label) via a
// deterministic deposit SourceID, so webhook/poll re-delivery never double-grants.
func (s *MoneyService) GrantPurchaseCredits(ctx context.Context, params GrantPurchaseCreditsParams) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	if params.Payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	if params.PaymentID == uuid.Nil {
		return fmt.Errorf("payment_id required")
	}
	if strings.TrimSpace(params.Source) == "" {
		return fmt.Errorf("source required")
	}
	if len(params.Spec) == 0 {
		return nil
	}

	payer := params.Payer
	return s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		now := s.now()
		for label, spec := range params.Spec {
			label = strings.TrimSpace(label)
			if err := s.validateCreditGrantSpec(ctx, label, spec); err != nil {
				return err
			}
			grantKey := fmt.Sprintf("openrails:purchase_credit_grant:%s:%s", params.PaymentID, label)
			grantID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(grantKey))
			if _, err := s.depositTx(ctx, q, DepositParams{
				TenantSubjectID: &payer,
				Actor:           payer.String(),
				Currency:        spec.UnitOrDefault(),
				Amount:          spec.Amount,
				Source:          strings.TrimSpace(params.Source),
				SourceID:        &grantID,
				ExpiresAt:       grantExpiry(now, spec),
			}); err != nil {
				return err
			}
			log.WithContext(ctx).WithFields(log.Fields{
				"payment_id":  params.PaymentID,
				"grant_label": label,
				"unit":        spec.UnitOrDefault(),
				"amount":      spec.Amount,
				"expiry_days": spec.EffectiveExpiryDays(),
				"grant_id":    grantID,
			}).Info("purchase credit grant applied")
		}
		return nil
	})
}
