package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// The two-level default (#1168): a card subscription charges its own card,
// else its customer's default card for the currency of its price. The rule is
// also billing.subscription_payment_method_id, which SQL reads use.

// FollowsDefault reports whether sub, having no card of its own, charges its
// customer's default card: an NMI subscription, or a Stripe one OpenRails
// collects.
func FollowsDefault(sub *models.Subscription) bool {
	return sub.PaymentMethodID == nil && CardSubscription(sub)
}

// CardSubscription reports whether sub is paid by a saved card OpenRails
// chooses: an NMI subscription (OpenRails or an NMI schedule collects it), or
// a Stripe one OpenRails collects.
func CardSubscription(sub *models.Subscription) bool {
	return (sub.Rail == models.RailNMI || sub.Rail == models.RailStripe) &&
		(sub.CollectionPolicy == models.CollectionPolicyEngine || sub.CollectionPolicy == models.CollectionPolicyNMISchedule)
}

// PaymentMethodOf is the card sub charges: its own, else the default it
// follows; nil when it has neither.
func PaymentMethodOf(ctx context.Context, q *gen.Queries, sub *models.Subscription) (*uuid.UUID, error) {
	if !FollowsDefault(sub) {
		return sub.PaymentMethodID, nil
	}
	var price *uuid.UUID
	if sub.PriceID != uuid.Nil {
		price = &sub.PriceID
	}
	return q.ResolveSubscriptionPaymentMethodID(ctx, gen.ResolveSubscriptionPaymentMethodIDParams{
		MerchantID: sub.MerchantID, CustomerID: sub.CustomerID, PriceID: price,
		Rail: string(sub.Rail), CollectionPolicy: string(sub.CollectionPolicy),
	})
}

// ownCardAtPurchase is the card a subscription bought with method keeps as
// its own: none when method is already the customer's default for its
// currency, which it then follows.
func ownCardAtPurchase(ctx context.Context, d *db.DB, sub *models.Subscription, method uuid.UUID) (*uuid.UUID, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	following := *sub
	following.MerchantID, following.PaymentMethodID = mid.UUID(), nil
	def, err := PaymentMethodOf(ctx, d.Gen(ctx), &following)
	if err != nil || (def != nil && *def == method) {
		return nil, err
	}
	return &method, nil
}

// paymentMethodsOf is the card each of subs charges, by subscription; prices
// are the subscriptions' prices by id.
func paymentMethodsOf(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, subs []*models.Subscription, prices map[uuid.UUID]*models.Price) (map[uuid.UUID]uuid.UUID, error) {
	out := make(map[uuid.UUID]uuid.UUID, len(subs))
	var customers []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, s := range subs {
		switch {
		case s.PaymentMethodID != nil:
			out[s.ID] = *s.PaymentMethodID
		case FollowsDefault(s) && !seen[s.CustomerID]:
			seen[s.CustomerID] = true
			customers = append(customers, s.CustomerID)
		}
	}
	if len(customers) == 0 {
		return out, nil
	}
	rows, err := q.ListDefaultPaymentMethodsByCustomers(ctx, gen.ListDefaultPaymentMethodsByCustomersParams{MerchantID: merchantID, CustomerIds: customers})
	if err != nil {
		return nil, err
	}
	type key struct {
		customer uuid.UUID
		currency string
	}
	defaults := make(map[key]uuid.UUID, len(rows))
	for _, r := range rows {
		defaults[key{r.CustomerID, r.Currency}] = r.DefaultPaymentMethodID
	}
	for _, s := range subs {
		price := prices[s.PriceID]
		if !FollowsDefault(s) || price == nil {
			continue
		}
		if id, ok := defaults[key{s.CustomerID, price.Currency}]; ok {
			out[s.ID] = id
		}
	}
	return out, nil
}

// ErrDefaultPaymentMethodRequired: the card a subscription would follow does
// not exist; the customer has no default for its currency.
var ErrDefaultPaymentMethodRequired = apperr.New(http.StatusBadRequest, billing.CodeDefaultPaymentMethodRequired, "the customer has no default card for the subscription's currency")

// ErrDefaultPaymentMethodInvalid: the card cannot be the customer's default.
var ErrDefaultPaymentMethodInvalid = apperr.New(http.StatusBadRequest, billing.CodeDefaultPaymentMethodInvalid, "the card cannot be the customer's default for the currency")

// ErrCardNotForSubscription: the card cannot pay a subscription through its
// provider account (another rail, or another account's vault).
var ErrCardNotForSubscription = apperr.New(http.StatusConflict, billing.CodePaymentMethodPSPMismatch, "the card belongs to another provider account than the subscription; add the card on the subscription's provider")

// PaymentSourceSwapper repoints an NMI schedule's payment source through the
// durable swap intent: EnqueueSwap in the caller's transaction, ExecuteSwap
// once it commits.
type PaymentSourceSwapper interface {
	EnqueueSwap(ctx context.Context, tx pgx.Tx, sub *models.Subscription, target gen.BillingPaymentMethod, swap PaymentSourceSwap) (uuid.UUID, error)
	ExecuteSwap(ctx context.Context, intentID uuid.UUID) error
}

// PaymentSourceSwap is one schedule's move onto a card.
type PaymentSourceSwap struct {
	// Follow: the subscription follows its customer's default once moved;
	// otherwise the card becomes its own.
	Follow bool
	// Old is the card the schedule bills now.
	Old *uuid.UUID
	// Verified is a recurring verification on the card, the storing
	// transaction the moved agreement cites when the card has no recurring
	// lineage of its own.
	Verified string
}

// DefaultPaymentMethodChange makes a saved card a customer's default for one
// currency.
type DefaultPaymentMethodChange struct {
	Customer        uuid.UUID
	Currency        string
	PaymentMethodID uuid.UUID
	// Verify runs the one recurring verification the move needs when the
	// card has no recurring agreement on the subscriptions' account.
	Verify RecurringVerifier
	// Swaps follows NMI schedules onto the card.
	Swaps PaymentSourceSwapper
}

// SetDefaultPaymentMethod makes a saved card the customer's default for one
// currency: it collects their invoices there (its unscheduled mandate) and
// pays every card subscription in the currency without a card of its own.
// One transaction moves those subscriptions' recurring mandates onto it,
// citing one storing verification run first if the card has no recurring
// lineage; NMI schedules follow through durable swap intents, run after it
// commits. A subscription the card cannot pay refuses the change.
func (s *SubscriptionLifecycleService) SetDefaultPaymentMethod(ctx context.Context, c DefaultPaymentMethodChange) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	c.Currency = strings.ToUpper(strings.TrimSpace(c.Currency))
	if c.Customer == uuid.Nil || c.PaymentMethodID == uuid.Nil || c.Currency == "" {
		return errors.New("default payment method: customer, card and currency are required")
	}
	var verified *charge.Mandate
	for {
		var needs *verification
		var swaps []uuid.UUID
		err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			needs, swaps, err = s.setDefault(ctx, tx, mid.UUID(), c, verified)
			return err
		})
		if err != nil {
			return err
		}
		if needs == nil {
			for _, id := range swaps {
				// Durable: the executor finishes what does not settle inline.
				if err := c.Swaps.ExecuteSwap(ctx, id); err != nil {
					log.WithContext(ctx).WithError(err).WithField("intent_id", id).Warn("default payment method: schedule swap left to the executor")
				}
			}
			return nil
		}
		if verified, err = s.verifyRecurring(ctx, c.Verify, *needs); err != nil {
			return err
		}
	}
}

// followMove is one subscription a default change moves.
type followMove struct {
	sub     *models.Subscription
	lineage *charge.Mandate
	swap    bool
}

func (s *SubscriptionLifecycleService) setDefault(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, c DefaultPaymentMethodChange, verified *charge.Mandate) (*verification, []uuid.UUID, error) {
	d := s.DB.NewWithPgxTx(tx)
	q := d.Gen(ctx)
	if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: merchantID, ID: c.Customer}); err != nil {
		return nil, nil, err
	}
	method, err := q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: merchantID, ID: c.PaymentMethodID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && method.CustomerID != c.Customer) {
		return nil, nil, apperr.New(http.StatusNotFound, billing.CodeResourceNotFound, "payment method not found")
	}
	if err != nil {
		return nil, nil, err
	}
	if !paymentmethods.Chargeable(method) {
		return nil, nil, fmt.Errorf("%w: the card cannot be charged", ErrDefaultPaymentMethodInvalid)
	}
	if descriptor, ok := rails.Lookup(models.Rail(method.Rail)); !ok || !descriptor.SupportsChargeSavedMethod {
		return nil, nil, fmt.Errorf("%w: rail %q charges no saved card", ErrDefaultPaymentMethodInvalid, method.Rail)
	}
	psp, err := charge.RoutePSP(ctx, q, method)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrDefaultPaymentMethodInvalid, err)
	}
	// The default carries the customer's consent to collection in the
	// currency: its unscheduled mandate cites the card's own lineage.
	unscheduled, err := mandates.Citable(ctx, q, merchantID, c.Customer, method.ID, psp, method.Rail, charge.AgreementUnscheduled)
	if err != nil {
		return nil, nil, err
	}
	if unscheduled == nil {
		return nil, nil, fmt.Errorf("%w: automatic collection: %w", ErrDefaultPaymentMethodInvalid, charge.ErrAgreementRequired)
	}
	var previous *uuid.UUID
	settings, err := q.GetMoneyAccountSettings(ctx, gen.GetMoneyAccountSettingsParams{MerchantID: merchantID, CustomerID: c.Customer, Currency: c.Currency})
	switch {
	case err == nil:
		previous = settings.DefaultPaymentMethodID
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, nil, err
	}
	rows, err := q.ListSubscriptionsFollowingDefault(ctx, gen.ListSubscriptionsFollowingDefaultParams{MerchantID: merchantID, CustomerID: c.Customer, Currency: c.Currency})
	if err != nil {
		return nil, nil, err
	}
	// Every move is checked before anything is written.
	moves := make([]followMove, 0, len(rows))
	for _, row := range rows {
		sub, err := models.SubscriptionFromGen(row)
		if err != nil {
			return nil, nil, err
		}
		move := followMove{sub: sub}
		switch sub.CollectionPolicy {
		case models.CollectionPolicyEngine:
			if err := RefuseOwnedRebillTerms(ctx, d, sub); err != nil {
				return nil, nil, err
			}
			if _, err := s.engineCard(ctx, d, sub, method.ID); err != nil {
				return nil, nil, err
			}
		case models.CollectionPolicyNMISchedule:
			if !sub.Status.Live() || (previous != nil && *previous == method.ID) {
				continue
			}
			target, err := models.PaymentMethodFromGen(method)
			if err != nil {
				return nil, nil, err
			}
			if ValidatePaymentMethodProviderAccount(target, sub) != nil {
				return nil, nil, fmt.Errorf("%w: subscription %s", ErrCardNotForSubscription, billing.SubscriptionID(sub.ID))
			}
			if err := ValidatePaymentMethodSourceCustody(target); err != nil {
				return nil, nil, err
			}
			if c.Swaps == nil {
				return nil, nil, errors.New("default payment method: NMI schedule swaps are not wired")
			}
			move.swap = true
		default:
			continue
		}
		lineage, needs, err := recurringLineage(ctx, q, sub, method, verified)
		if err != nil || needs != nil {
			return needs, nil, err
		}
		move.lineage = lineage
		moves = append(moves, move)
	}

	now := s.now()
	if err := q.InsertMoneyAccountSettingsIfAbsent(ctx, gen.InsertMoneyAccountSettingsIfAbsentParams{MerchantID: merchantID, CustomerID: c.Customer, Currency: c.Currency, BillingMode: "prepaid", Now: now}); err != nil {
		return nil, nil, err
	}
	if n, err := q.SetMoneyAccountDefaultPaymentMethod(ctx, gen.SetMoneyAccountDefaultPaymentMethodParams{MerchantID: merchantID, CustomerID: c.Customer, Currency: c.Currency, PaymentMethodID: &method.ID, Now: now}); err != nil {
		return nil, nil, err
	} else if n != 1 {
		return nil, nil, errors.New("default payment method: settings row not found")
	}
	if _, err := mandates.Replace(ctx, q, mandates.Agreement{MerchantID: merchantID, CustomerID: c.Customer, PaymentMethodID: method.ID, PSPID: psp, Rail: method.Rail,
		Kind: charge.AgreementUnscheduled, Currency: c.Currency, Lineage: unscheduled, AcceptedAt: now}, now); err != nil {
		return nil, nil, err
	}
	var swaps []uuid.UUID
	for _, m := range moves {
		if !m.swap {
			if err := s.moveRecurring(ctx, d, m.sub, method.ID, m.lineage, now); err != nil {
				return nil, nil, err
			}
			continue
		}
		swap := PaymentSourceSwap{Follow: true, Old: previous}
		if verified != nil && m.lineage == verified {
			swap.Verified = verified.InitialTransactionID
		}
		id, err := c.Swaps.EnqueueSwap(ctx, tx, m.sub, method, swap)
		if err != nil {
			return nil, nil, err
		}
		swaps = append(swaps, id)
	}
	// Choosing a card is what a stopped collection waited for.
	if _, err := q.ResumeStoppedInvoiceCollection(ctx, gen.ResumeStoppedInvoiceCollectionParams{MerchantID: merchantID, CustomerID: c.Customer, Currency: c.Currency, Now: now}); err != nil {
		return nil, nil, err
	}
	return nil, swaps, nil
}

// NMIScheduleMove is the move of an NMI schedule onto a card.
type NMIScheduleMove struct {
	Target uuid.UUID
	Swap   PaymentSourceSwap
}

// PlanNMIScheduleMove resolves the card an NMI schedule moves to: methodID,
// or, with methodID nil, the customer's default it then follows. A move onto
// the card the schedule already bills only changes whether that card is the
// subscription's own; it is applied here and the plan is nil.
func (s *SubscriptionLifecycleService) PlanNMIScheduleMove(ctx context.Context, subscriptionID uuid.UUID, methodID *uuid.UUID) (*NMIScheduleMove, error) {
	var plan *NMIScheduleMove
	err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.DB.NewWithPgxTx(tx)
		repo := NewSubscriptionRepo(d)
		sub, err := repo.GetByIDForUpdate(ctx, subscriptionID)
		if err != nil {
			return err
		}
		billed, err := PaymentMethodOf(ctx, d.Gen(ctx), sub)
		if err != nil {
			return err
		}
		wanted := *sub
		wanted.PaymentMethodID = methodID
		target, err := PaymentMethodOf(ctx, d.Gen(ctx), &wanted)
		if err != nil {
			return err
		}
		if target == nil {
			return ErrDefaultPaymentMethodRequired
		}
		if billed == nil || *billed != *target {
			plan = &NMIScheduleMove{Target: *target, Swap: PaymentSourceSwap{Follow: methodID == nil, Old: billed}}
			return nil
		}
		if equalID(sub.PaymentMethodID, methodID) {
			return nil
		}
		sub.PaymentMethodID = methodID
		return repo.UpdateAt(ctx, sub, s.now())
	})
	return plan, err
}

// VerifyNMIScheduleMove runs the recurring verification a schedule's move
// needs when the target card has no recurring lineage on its account; the
// moved agreement cites it.
func (s *SubscriptionLifecycleService) VerifyNMIScheduleMove(ctx context.Context, sub *models.Subscription, move *NMIScheduleMove, verify RecurringVerifier) error {
	var needs *verification
	err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := s.DB.NewWithPgxTx(tx).Gen(ctx)
		method, err := q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: sub.MerchantID, ID: move.Target})
		if err != nil {
			return err
		}
		if !paymentmethods.Chargeable(method) {
			return ErrCardUnusable
		}
		_, needs, err = recurringLineage(ctx, q, sub, method, nil)
		return err
	})
	if err != nil || needs == nil {
		return err
	}
	verified, err := s.verifyRecurring(ctx, verify, *needs)
	if err != nil {
		return err
	}
	move.Swap.Verified = verified.InitialTransactionID
	return nil
}

func equalID(a, b *uuid.UUID) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
