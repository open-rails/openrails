// Package attempts records every authorization a PSP answered (#1110): the $0
// card verification, sales, rebills and retries, approved or not, classified by
// internal/billing/decline. It is written where the answer is retained, in the
// same transaction.
package attempts

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/billing/decline"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Kind is what an attempt tries to do.
type Kind string

const (
	// Verify is the $0 card verification at card save or replacement.
	Verify  Kind = "verify"
	Initial Kind = "initial"
	Upgrade Kind = "upgrade"
	// Rebill is a cycle's first attempt, whoever sent it.
	Rebill        Kind = "rebill"
	DunningRetry  Kind = "dunning_retry"
	CustomerRetry Kind = "customer_retry"
	Invoice       Kind = "invoice"
)

// Owner is who collects the subscription an attempt is for.
type Owner string

const (
	OwnerEngine      Owner = "engine"
	OwnerNMISchedule Owner = "nmi_schedule"
	OwnerProvider    Owner = "provider"
	OwnerNone        Owner = "none"
)

// OwnerOf is the owner of a subscription's collection.
func OwnerOf(policy models.CollectionPolicy) Owner {
	switch policy {
	case models.CollectionPolicyEngine:
		return OwnerEngine
	case models.CollectionPolicyNMISchedule:
		return OwnerNMISchedule
	case models.CollectionPolicyProvider:
		return OwnerProvider
	}
	return OwnerNone
}

// CardSave is the checkout target of a card saved without a purchase.
const CardSave = "card_save"

// checkoutGap ends a checkout that went quiet.
const checkoutGap = time.Hour

// Attempt is one answered authorization.
type Attempt struct {
	MerchantID, CustomerID, PSPID uuid.UUID
	Rail                          string
	Kind                          Kind
	// Owner defaults to none.
	Owner Owner
	// NewCard: the card was entered in this checkout or card add. A charge in
	// the checkout that verified its card is new without saying so.
	NewCard bool
	// ProviderSchedule: the PSP charged on its own schedule.
	ProviderSchedule bool
	// ObservedVia is how OpenRails learned the answer: "" (the PSP's reply to
	// our request), "webhook" or "pull".
	ObservedVia string
	Approved    bool
	// Answer is the PSP's code, AVS/CVV letters and text (Rail is set here).
	Answer        decline.Evidence
	TransactionID string
	// Amount is in micros; a verification has 0 and no currency.
	Amount   int64
	Currency string
	At       time.Time
	// Target groups a buyer's new-card attempts into one checkout: a price id,
	// or CardSave. Empty for rebills.
	Target                                                   string
	SubscriptionID, PaymentMethodID, PaymentID, RailIntentID *uuid.UUID
	// Step keys an operation's attempts that carry no transaction id.
	Step                           string
	CardBrand, CardLast4, TokenType string
}

var last4Shape = regexp.MustCompile(`^[0-9]{4}$`)

// Record writes an attempt once: a replay with the same transaction id, or the
// same operation step, is a no-op.
func Record(ctx context.Context, q *gen.Queries, a Attempt) error {
	if q == nil || a.MerchantID == uuid.Nil || a.CustomerID == uuid.Nil || a.PSPID == uuid.Nil || a.Kind == "" || a.At.IsZero() {
		return errors.New("attempt: merchant, customer, PSP, kind and time are required")
	}
	rail := strings.ToLower(strings.TrimSpace(a.Rail))
	a.Answer.Rail = rail
	row := gen.InsertPaymentAttemptParams{
		ID: uuidutil.NewV7(), MerchantID: a.MerchantID, CustomerID: a.CustomerID, PspID: a.PSPID, Rail: rail,
		Kind: string(a.Kind), Owner: string(OwnerNone), CardEntry: "saved", Source: "openrails", ObservedVia: "response",
		Amount: a.Amount, Currency: optional(strings.ToUpper(a.Currency)), AttemptedAt: a.At.UTC(),
		SubscriptionID: a.SubscriptionID, PaymentMethodID: a.PaymentMethodID, PaymentID: a.PaymentID,
		RailIntentID: a.RailIntentID, Step: a.Step,
		TransactionID: optional(a.TransactionID), AvsResult: optional(a.Answer.AVS), CvvResult: optional(a.Answer.CVV),
		ResponseText: optional(truncate(a.Answer.Text, 128)), CardBrand: optional(strings.ToLower(a.CardBrand)),
	}
	if a.Owner != "" {
		row.Owner = string(a.Owner)
	}
	if a.NewCard {
		row.CardEntry = "new"
	}
	if a.ProviderSchedule {
		row.Source = "provider_schedule"
	}
	if a.ObservedVia != "" {
		row.ObservedVia = a.ObservedVia
	}
	if last4Shape.MatchString(a.CardLast4) {
		row.CardLast4 = &a.CardLast4
	}
	switch a.TokenType {
	case "network_token", "pan_via_proxy", "psp_token":
		row.TokenType = &a.TokenType
	}
	if a.Approved {
		row.Category = string(decline.Approved)
		row.ResponseCode = optional(a.Answer.Code)
	} else {
		verdict := decline.ClassifyEvidence(a.Answer)
		reason, action := string(verdict.Reason), verdict.Action.String()
		row.Category, row.Reason, row.Action = string(verdict.Category), &reason, &action
		row.ResponseCode = optional(verdict.Code)
	}
	if a.Target != "" {
		checkout, err := checkoutFor(ctx, q, a)
		if err != nil {
			return err
		}
		row.CheckoutID, row.CheckoutTarget = &checkout, &a.Target
		if !a.NewCard && a.Kind != Verify && a.PaymentMethodID != nil {
			verified, err := q.CheckoutVerifiedPaymentMethod(ctx, gen.CheckoutVerifiedPaymentMethodParams{
				MerchantID: a.MerchantID, CustomerID: a.CustomerID, CheckoutTarget: a.Target, CheckoutID: checkout, PaymentMethodID: *a.PaymentMethodID,
			})
			if err != nil {
				return err
			}
			if verified {
				row.CardEntry = "new"
			}
		}
	}
	_, err := q.InsertPaymentAttempt(ctx, row)
	return err
}

// checkoutFor continues the buyer's open checkout on this target, or opens a
// new one. A checkout ends when its target is approved or it goes quiet.
func checkoutFor(ctx context.Context, q *gen.Queries, a Attempt) (uuid.UUID, error) {
	last, err := q.LatestPaymentCheckoutAttempt(ctx, gen.LatestPaymentCheckoutAttemptParams{
		MerchantID: a.MerchantID, CustomerID: a.CustomerID, CheckoutTarget: a.Target, Since: a.At.Add(-checkoutGap),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuidutil.NewV7(), nil
	}
	if err != nil {
		return uuid.Nil, err
	}
	if last.Category == string(decline.Approved) && closes(Kind(last.Kind), a.Target) {
		return uuidutil.NewV7(), nil
	}
	return last.CheckoutID, nil
}

// closes reports whether an approval of kind completes the target.
func closes(kind Kind, target string) bool {
	if target == CardSave {
		return kind == Verify
	}
	return kind == Initial || kind == Upgrade
}

func optional(s string) *string {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	return &s
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	for len(s) > n || !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
		if len(s) > n {
			s = s[:n]
		}
	}
	return s
}
