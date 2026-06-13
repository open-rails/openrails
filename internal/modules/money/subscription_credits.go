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
	log "github.com/sirupsen/logrus"
)

type GrantSubscriptionCreditsParams struct {
	SubscriptionID uuid.UUID
	PeriodEnd      time.Time
	Cadence        models.CreditGrantCadence // once|per_renewal
	Source         string                    // for deposit transaction (e.g., "subscription_initial", "subscription_renewal")
}

// validateCreditGrantSpec validates one promo-money grant spec. The grant key is
// just a label now (#472: money has no credit_type); a non-empty key still scopes
// the per-grant idempotency.
func validateCreditGrantSpec(grantKey string, spec models.CreditGrantSpec) error {
	if strings.TrimSpace(grantKey) == "" {
		return fmt.Errorf("grant key is empty")
	}
	if spec.Amount <= 0 {
		return fmt.Errorf("invalid credits_spec: %s amount must be > 0", grantKey)
	}
	if spec.ExpiresDays != nil && *spec.ExpiresDays <= 0 {
		return fmt.Errorf("invalid credits_spec: %s expires_days must be > 0", grantKey)
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
			if err := validateCreditGrantSpec(label, spec); err != nil {
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

			var expiresAt *time.Time
			if spec.ExpiresDays != nil && *spec.ExpiresDays > 0 {
				t := now.Add(time.Duration(*spec.ExpiresDays) * 24 * time.Hour)
				expiresAt = &t
			}

			if _, err := s.depositTx(ctx, q, DepositParams{
				Actor:     sub.TenantSubjectID.String(),
				Amount:    spec.Amount,
				Source:    strings.TrimSpace(params.Source),
				SourceID:  &grantID,
				ExpiresAt: expiresAt,
			}); err != nil {
				return err
			}

			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": sub.ID,
				"period_end":      params.PeriodEnd.UTC(),
				"grant_label":     label,
				"amount":          spec.Amount,
				"expires_days":    spec.ExpiresDays,
				"cadence":         cadence,
				"grant_id":        grantID,
			}).Info("subscription money grant applied")
		}

		return nil
	})
}
