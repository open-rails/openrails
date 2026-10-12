package payment

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// initialMembershipQuoteFingerprint excludes newly allocated IDs and acceptance
// instants. A replay compares the customer's displayed commercial decision.
func initialMembershipQuoteFingerprint(terms subscriptions.InitialMembershipTerms) string {
	duration := terms.PeriodEnd.Sub(terms.PeriodStart)
	// Older accepted quotes implied access for exactly one billing period.
	// Keep that equivalent decision's original hash while distinguishing both
	// indefinite access and independently sized access windows on new orders.
	var access json.RawMessage
	if terms.AccessDurationHours == nil || duration%time.Hour != 0 || time.Duration(*terms.AccessDurationHours) != duration/time.Hour {
		access, _ = json.Marshal(terms.AccessDurationHours)
	}
	legacy := terms.LegacyEntitlements
	terms.LegacyEntitlements = nil
	terms.SubscriptionID, terms.PaymentID = uuid.Nil, uuid.Nil
	terms.AcceptedAt, terms.PeriodStart, terms.PeriodEnd = time.Time{}, time.Time{}, time.Time{}
	type fingerprintTerms struct {
		subscriptions.InitialMembershipTerms
		Entitlements        any                               `json:"entitlements,omitempty"`
		Replaces            *subscriptions.ReplacedMembership `json:"replaces,omitempty"`
		AccessDurationHours json.RawMessage                   `json:"access_duration_hours,omitempty"`
	}
	accepted, _ := grants.AcceptedEntitlementsValue(terms.Entitlements, legacy)
	terms.Entitlements = nil
	raw, _ := json.Marshal(struct {
		Terms    fingerprintTerms
		Duration time.Duration
	}{fingerprintTerms{InitialMembershipTerms: terms, Entitlements: accepted, Replaces: terms.Replaces, AccessDurationHours: access}, duration})
	digest := sha256.Sum256(raw)
	return fmt.Sprintf("%x", digest)
}

// enrollmentInstrument freezes a new subscription's card and the recurring
// lineage its enrollment cites: the card's on the account, or none when the
// enrollment charge stores it. On Stripe every enrollment's PaymentIntent
// stores its own agreement (setup_future_usage), and Stripe links the rest.
func enrollmentInstrument(ctx context.Context, q *gen.Queries, method gen.BillingPaymentMethod, psp uuid.UUID) (charge.FrozenInstrument, error) {
	instrument := charge.FreezeInstrument(method, psp)
	if method.Rail == string(models.RailStripe) {
		return instrument, nil
	}
	var err error
	instrument.Mandate, err = mandates.Citable(ctx, q, method.MerchantID, method.CustomerID, method.ID, psp, method.Rail, charge.AgreementRecurring)
	return instrument, err
}
