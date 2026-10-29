package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders/usage"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// ErrAgreementNotFound answers an agreement that is not the customer's live
// one.
var ErrAgreementNotFound = apperr.New(http.StatusNotFound, billing.CodeResourceNotFound, "agreement not found")

// openUsageOrderLimit bounds the open usage orders a revocation stops: each
// is a closed billing window, so a customer has a few.
const openUsageOrderLimit = 100

// ListAgreements is one page of the customer's live charge agreements,
// newest first.
func (s *Service) ListAgreements(ctx context.Context, customer billing.CustomerID, page billing.PageRequest) (billing.ListPage[billing.Agreement], error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return billing.ListPage[billing.Agreement]{}, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return billing.ListPage[billing.Agreement]{}, err
	}
	limit, err := pagination.Limit(page)
	if err != nil {
		return billing.ListPage[billing.Agreement]{}, err
	}
	type position struct {
		At time.Time `json:"a"`
		I  uuid.UUID `json:"i"`
	}
	var after position
	present, err := pagination.Decode(page.Cursor, &after)
	if err != nil {
		return billing.ListPage[billing.Agreement]{}, err
	}
	params := gen.ListLiveAgreementsPageParams{MerchantID: mid.UUID(), CustomerID: customer.UUID(), RowLimit: pagination.Fetch(limit)}
	if present {
		params.AfterAt, params.AfterID = &after.At, &after.I
	}
	rows, err := s.rt.DB.Gen(ctx).ListLiveAgreementsPage(ctx, params)
	if err != nil {
		return billing.ListPage[billing.Agreement]{}, err
	}
	cut := pagination.Cut(rows, limit, func(m gen.BillingMandate) any { return position{At: m.CreatedAt, I: m.ID} })
	items, err := s.agreements(ctx, customer, cut.Items)
	return billing.ListPage[billing.Agreement]{Items: items, Next: cut.Next}, err
}

// agreements are mandates as their customer reads them: the card and, for a
// recurring one, its subscription's terms.
func (s *Service) agreements(ctx context.Context, customer billing.CustomerID, rows []gen.BillingMandate) ([]billing.Agreement, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var methodIDs, subIDs []uuid.UUID
	for _, m := range rows {
		if m.PaymentMethodID != nil {
			methodIDs = append(methodIDs, *m.PaymentMethodID)
		}
		if m.SubscriptionID != nil {
			subIDs = append(subIDs, *m.SubscriptionID)
		}
	}
	cards := map[uuid.UUID]*billing.CardDetails{}
	if len(methodIDs) > 0 {
		methods, err := s.rt.DB.Gen(ctx).ListCustomerPaymentMethodsByIDs(ctx, gen.ListCustomerPaymentMethodsByIDsParams{MerchantID: mid.UUID(), Ids: methodIDs, CustomerID: customer.UUID()})
		if err != nil {
			return nil, err
		}
		for _, pm := range methods {
			cards[pm.ID] = models.CardFromColumns(pm.CardBrand, pm.CardLast4, pm.CardExpMonth, pm.CardExpYear).Details()
		}
	}
	subs := map[uuid.UUID]billing.Subscription{}
	if len(subIDs) > 0 {
		page, err := s.rt.UserSubscriptionService.ListUserSubscriptions(ctx, customer.String(), subscriptions.GetSubscriptionsFilters{IDs: subIDs}, billing.PageRequest{Limit: len(subIDs)})
		if err != nil {
			return nil, err
		}
		for _, sub := range page.Items {
			view := sub.View()
			subs[view.ID.UUID()] = view
		}
	}
	out := make([]billing.Agreement, 0, len(rows))
	for _, m := range rows {
		a := billing.Agreement{ID: billing.MandateID(m.ID), Kind: billing.MandateKind(m.Kind), Status: billing.MandateStatus(m.Status), AcceptedAt: m.AcceptedAt.UTC()}
		if m.EndedAt != nil {
			at := m.EndedAt.UTC()
			a.EndedAt = &at
		}
		if m.Currency != nil {
			a.Currency = *m.Currency
		}
		if m.PaymentMethodID != nil {
			id := billing.PaymentMethodID(*m.PaymentMethodID)
			a.PaymentMethodID, a.Card = &id, cards[*m.PaymentMethodID]
		}
		if m.SubscriptionID != nil {
			sub, ok := subs[*m.SubscriptionID]
			terms := &billing.AgreementSubscription{ID: billing.SubscriptionID(*m.SubscriptionID)}
			if ok {
				terms.Product, terms.NextChargeAt = agreementTerms(sub, terms)
				if sub.Price != nil {
					a.Currency = sub.Price.Currency
				}
			}
			a.Subscription = terms
		}
		out = append(out, a)
	}
	return out, nil
}

// agreementTerms fills what a subscription charges: its amount every
// interval, next at its period's end while it renews.
func agreementTerms(sub billing.Subscription, terms *billing.AgreementSubscription) (string, *time.Time) {
	product := ""
	if sub.Product != nil {
		product = sub.Product.DisplayName
	}
	if sub.Price != nil {
		quantity := int64(1)
		if sub.Quantity != nil && *sub.Quantity > 0 {
			quantity = int64(*sub.Quantity)
		}
		terms.Amount = sub.Price.UnitAmount * quantity
		terms.BillingIntervalHours = sub.Price.BillingIntervalHours
	}
	if sub.CancelScheduled || sub.Status == billing.SubscriptionCanceled || sub.CurrentPeriodEndsAt == nil {
		return product, nil
	}
	next := sub.CurrentPeriodEndsAt.UTC()
	return product, &next
}

// RevokeAgreement revokes one of the customer's live charge agreements: the
// merchant charges its card under it no more. A recurring one cancels its
// subscription at the period's end, through the customer's own cancel; when
// that cannot run (a provider-billed schedule while the merchant's
// destructive actions are disarmed) nothing is revoked. An unscheduled one
// bills the usage owed until now as an open usage order the customer pays
// themselves, stops collecting every open usage order in its currency, and
// leaves only the balance to spend there until the customer gives another.
// The merchant hears mandate.revoked; the customer gets the confirmation.
func (s *Service) RevokeAgreement(ctx context.Context, customer billing.CustomerID, id billing.MandateID) (billing.AgreementRevocation, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return billing.AgreementRevocation{}, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return billing.AgreementRevocation{}, err
	}
	m, err := s.rt.DB.Gen(ctx).GetLiveAgreement(ctx, gen.GetLiveAgreementParams{MerchantID: mid.UUID(), ID: id.UUID(), CustomerID: customer.UUID()})
	switch {
	case db.IsNotFound(err):
		return billing.AgreementRevocation{}, ErrAgreementNotFound
	case err != nil:
		return billing.AgreementRevocation{}, err
	}
	var out billing.AgreementRevocation
	switch billing.MandateKind(m.Kind) {
	case billing.MandateRecurring:
		if out.Subscription, err = s.revokeRecurring(ctx, customer, m); err != nil {
			return billing.AgreementRevocation{}, err
		}
	case billing.MandateUnscheduled:
		if out.UsageOrder, err = s.revokeUnscheduled(ctx, customer, m); err != nil {
			return billing.AgreementRevocation{}, err
		}
	default:
		return billing.AgreementRevocation{}, ErrAgreementNotFound
	}
	revoked, err := s.rt.DB.Gen(ctx).GetMandate(ctx, gen.GetMandateParams{MerchantID: mid.UUID(), ID: m.ID})
	if err != nil {
		return billing.AgreementRevocation{}, err
	}
	views, err := s.agreements(ctx, customer, []gen.BillingMandate{revoked})
	if err != nil {
		return billing.AgreementRevocation{}, err
	}
	out.Agreement = views[0]
	return out, nil
}

// revokeRecurring cancels the agreement's subscription at the period's end,
// as the customer's own cancel does, then revokes the agreement.
func (s *Service) revokeRecurring(ctx context.Context, customer billing.CustomerID, m gen.BillingMandate) (*billing.Subscription, error) {
	sub, err := s.rt.SubscriptionService.GetByID(ctx, *m.SubscriptionID)
	if err != nil {
		return nil, err
	}
	if sub.Status != models.StatusCanceled {
		if err := s.CancelSubscriptionForCustomer(ctx, customer, sub, "payment agreement revoked"); err != nil {
			return nil, err
		}
	}
	page, err := s.rt.UserSubscriptionService.ListUserSubscriptions(ctx, customer.String(), subscriptions.GetSubscriptionsFilters{IDs: []uuid.UUID{sub.ID}}, billing.PageRequest{Limit: 1})
	if err != nil || len(page.Items) == 0 {
		return nil, errors.Join(err, errors.New("revoked agreement: subscription not found"))
	}
	view := page.Items[0].View()
	currency := ""
	if view.Price != nil {
		currency = view.Price.Currency
	}
	if err := s.endAgreement(ctx, m, currency, nil); err != nil {
		return nil, err
	}
	return &view, nil
}

// revokeUnscheduled bills the usage owed until now, then in one transaction
// revokes the agreement, stops collecting the currency's open usage orders,
// closes the customer's credit there and tells the customer what it owes.
func (s *Service) revokeUnscheduled(ctx context.Context, customer billing.CustomerID, m gen.BillingMandate) (*billing.Order, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	currency := ""
	if m.Currency != nil {
		currency = *m.Currency
	}
	now := s.now().UTC()
	var billed *gen.BillingOrder
	from, err := s.rt.DB.Gen(ctx).EarliestPendingOwedAccrual(ctx, gen.EarliestPendingOwedAccrualParams{MerchantID: mid.UUID(), CustomerID: customer.UUID(), Currency: currency})
	switch {
	case db.IsNotFound(err):
	case err != nil:
		return nil, err
	default:
		// The window closes just past now: usage accrued this instant is in it
		// (timestamps are microseconds).
		if billed, err = s.rt.MoneyService.FinalizeUsage(ctx, identity.CustomerID(customer), currency, from, now.Add(time.Microsecond)); err != nil {
			return nil, err
		}
	}
	err = s.endAgreement(ctx, m, currency, func(ctx context.Context, q *gen.Queries) error {
		if err := q.RevokeMoneyCollection(ctx, gen.RevokeMoneyCollectionParams{MerchantID: mid.UUID(), CustomerID: customer.UUID(), Currency: currency, Now: now}); err != nil {
			return err
		}
		// It now spends only its balance, which is watched as a prepaid one.
		if err := q.WatchCustomerBalance(ctx, gen.WatchCustomerBalanceParams{MerchantID: mid.UUID(), CustomerID: customer.UUID(), Currency: currency, Now: now}); err != nil {
			return err
		}
		open, err := q.ListOpenUsageOrdersOfCurrency(ctx, gen.ListOpenUsageOrdersOfCurrencyParams{MerchantID: mid.UUID(), CustomerID: customer.UUID(), Currency: currency, RowLimit: openUsageOrderLimit})
		if err != nil {
			return err
		}
		failure := billing.PaymentFailure{Reason: charge.AgreementRequiredCode, Message: "The customer revoked the agreement to charge its card for usage; it pays this order itself."}
		var due int64
		var latest *uuid.UUID
		for _, o := range open {
			due += usage.Due(o)
			latest = &o.ID
			if o.CollectionMethod == nil || *o.CollectionMethod != "charge_automatically" || o.NextCollectionAttemptAt == nil && o.CollectionFailureCount != nil && *o.CollectionFailureCount > 0 {
				continue
			}
			if err := usage.StopCollection(ctx, q, mid.UUID(), o.ID, failure, now); err != nil {
				return err
			}
		}
		data := billing.NotificationData{Reason: "agreement_revoked", Currency: currency, EffectiveAt: &now}
		if due > 0 {
			data.AmountDue = &due
		}
		if latest != nil {
			data.OrderID = billing.OrderID(*latest)
		}
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		return q.CreateNotificationIfAbsent(ctx, gen.CreateNotificationIfAbsentParams{
			ID:         uuidutil.DeterministicID(uuidutil.DeterministicNamespace, string(models.NotificationAgreementRevoked), mid.String(), m.ID.String()),
			MerchantID: mid.UUID(), CustomerID: customer.UUID(), EventType: string(models.NotificationAgreementRevoked), Data: raw, CreatedAt: now,
		})
	})
	if err != nil || billed == nil {
		return nil, err
	}
	order, err := s.GetOrder(ctx, OrderActor{Customer: customer}, billing.OrderID(billed.ID))
	if err != nil {
		return nil, err
	}
	return order, nil
}

// endAgreement revokes the agreement and tells the merchant, with the
// kind's own writes, in one transaction. currency is what it charged in.
func (s *Service) endAgreement(ctx context.Context, m gen.BillingMandate, currency string, also func(context.Context, *gen.Queries) error) error {
	now := s.now().UTC()
	return s.rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		n, err := q.EndMandate(ctx, gen.EndMandateParams{MerchantID: m.MerchantID, ID: m.ID, Reason: string(billing.MandateEndCustomerRevoked), Now: now})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrAgreementNotFound
		}
		event := billing.MandateHostEvent{MandateID: billing.MandateID(m.ID), Kind: billing.MandateKind(m.Kind), CustomerID: billing.CustomerID(m.CustomerID), Currency: currency, RevokedAt: now}
		if m.SubscriptionID != nil {
			sub := billing.SubscriptionID(*m.SubscriptionID)
			event.SubscriptionID = &sub
		}
		raw, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if err := q.EnqueueMandateRevoked(ctx, gen.EnqueueMandateRevokedParams{MerchantID: m.MerchantID, MandateID: m.ID, OccurredAt: now, Data: raw}); err != nil {
			return err
		}
		if also != nil {
			return also(ctx, q)
		}
		return nil
	})
}

// CancelSubscriptionForCustomer is the customer's own cancel at the
// period's end. A provider-billed schedule that cannot be deleted
// (destructive actions disarmed) is refused with its operator finding.
func (s *Service) CancelSubscriptionForCustomer(ctx context.Context, customer billing.CustomerID, sub *models.Subscription, reason string) error {
	if sub == nil || customer.IsZero() || sub.CustomerID != customer.UUID() {
		return subscriptions.ErrSubscriptionNotFound
	}
	rt, err := s.runtime()
	if err != nil {
		return err
	}
	if rt.SubscriptionService == nil {
		return fmt.Errorf("subscription service unavailable")
	}
	current, err := rt.SubscriptionService.GetByID(ctx, sub.ID)
	if err != nil {
		return err
	}
	if current.CustomerID != customer.UUID() || current.MerchantID != sub.MerchantID {
		return subscriptions.ErrSubscriptionNotFound
	}
	sub = current
	if sub.Status == models.StatusCanceled {
		return nil
	}
	if !sub.Status.Live() {
		return subscriptions.ErrSubscriptionNotActive
	}

	ctx = db.WithPSPID(ctx, sub.PspID)
	if _, err := subscriptions.RequireProviderCancelArmed(ctx, s.rt.SubscriptionService.Database(), sub, false); err != nil {
		return err
	}
	// A completed cancellation is a domain no-op, including after reservation pruning.
	if sub.Status != models.StatusCanceled {
		policy := config.UserActionRateLimitConfig{}
		if rt.Config != nil {
			policy = rt.Config.UserActionRateLimit
		}
		limiter := destructive.NewUserActionLimiter(rt.DB, policy)
		if err := limiter.Reserve(ctx, destructive.CancelSubscription, fmt.Sprintf("cancel:%s:%d", sub.ID, sub.LifecycleRev), sub.CustomerID); err != nil {
			return err
		}
	}

	return s.cancelForCustomer(ctx, customer, sub, reason)
}

// cancelForCustomer records the cancel at once. A provider-billed schedule is
// stopped by a durable provider intent committed with it, scheduled before
// expiry when the rail supports a cancellation undo window.
func (s *Service) cancelForCustomer(ctx context.Context, customer billing.CustomerID, sub *models.Subscription, reason string) error {
	if sub.CollectionPolicy != models.CollectionPolicyEngine {
		if s.rt.UserSubscriptionService == nil {
			return fmt.Errorf("user subscription service unavailable")
		}
		return s.rt.UserSubscriptionService.CancelUserSubscription(ctx, customer.String(), sub.ID, reason)
	}
	if s.rt.SubscriptionLifecycleService == nil {
		return fmt.Errorf("subscription lifecycle service unavailable")
	}
	return s.rt.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
		SubscriptionID:     &sub.ID,
		CancelType:         models.CancelTypeUser,
		CancelFeedback:     &reason,
		RefuseOwnedRenewal: true,
		Requested:          true,
	})
}
