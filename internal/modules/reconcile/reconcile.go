// Package reconcile holds the Stripe backfill orchestration shared by the
// standalone stripe-reconcile CLI and embedded hosts (via pkg/reconcile).
//
// It mirrors existing Stripe state into the local DB and NEVER charges or
// mutates anything in Stripe. Every write is create-if-not-exists / upsert /
// link-if-changed, so the whole run is idempotent and safe to re-run.
package reconcile

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
)

const stripeProcessorName = "stripe"

// Options controls the Stripe backfill run.
type Options struct {
	// Apply writes changes; when false (default) the run only reports.
	Apply bool
	// MaxPages bounds the pages fetched from Stripe (0 = unlimited).
	MaxPages int
	// PageSize bounds the per-request page size (Stripe caps at 100).
	PageSize int
	// SkipPayments skips the charges/payments/cards pass (subscriptions only).
	SkipPayments bool
	// UserExists reports whether a user_id (resolved from Stripe metadata /
	// customer mapping) exists in the host's identity system. When set, any
	// remote subscription or charge that resolves to an unknown user is skipped
	// (no membership/payment/card/customer rows are written) and reported -
	// there is no point storing billing for a user we don't have. Injected so
	// OpenRails stays decoupled from identity; the embedded host (cozy-art)
	// always wires it, making the skip unconditional in the product. When nil
	// (standalone with no identity source) the reconcile behaves as before.
	UserExists func(ctx context.Context, userID string) (bool, error)
}

// userKnown reports whether userID exists in the host identity system. With no
// checker wired it returns true (legacy behavior); the embedded host always
// wires one, so unknown-user skipping is unconditional there.
func (o Options) userKnown(ctx context.Context, userID string) (bool, error) {
	if o.UserExists == nil {
		return true, nil
	}
	return o.UserExists(ctx, userID)
}

// Report summarizes what the run observed and (when Apply) wrote. It is returned
// so embedded hosts can inspect the outcome instead of parsing stdout.
type Report struct {
	RemoteSubscriptionsFetched int
	RemoteActiveSubscriptions  int
	MappableSubscriptions      int
	SkippedSubscriptions       []string
	UnknownUserSubscriptions   []string
	LocalActiveSubscriptions   int
	RemoteOnlySubscriptions    []string
	LocalOnlySubscriptions     []string
	MatchedSubscriptions       int
	MembershipsCreated         int

	RemoteChargesFetched int
	ChargesMapped        int
	ChargesSkipped       []string
	UnknownUserCharges   []string
	PaymentsCreated      int
	PaymentCardsSnapshot int
	CardsUpserted        int
}

// priceResolver is the subset of catalog.PriceService the mapper needs. Keeping
// it as an interface lets the mapping logic be unit tested without a database.
type priceResolver interface {
	GetByID(ctx context.Context, id uuid.UUID) (*models.Price, error)
	GetByStripePriceID(ctx context.Context, stripePriceID string) (*models.Price, error)
}

// mappedSubscription is a remote Stripe subscription that resolved to a local
// user and price.
type mappedSubscription struct {
	Remote  subscriptions.StripeRemoteSubscription
	UserID  string
	PriceID uuid.UUID
}

// MapRemoteSubscription resolves a remote Stripe subscription to an OpenRails
// user and price. The user comes from metadata["user_id"]; the price resolves
// from metadata["internal_price_id"] (a local price UUID), falling back to the
// line-item Stripe price id. A non-empty reason means the sub could not be
// mapped and should be skipped/reported.
func mapRemoteSubscription(ctx context.Context, prices priceResolver, remote subscriptions.StripeRemoteSubscription) (*mappedSubscription, string) {
	userID := normalize.FirstNonEmpty(remote.Metadata["user_id"], remote.Metadata["userId"], remote.Metadata["uid"])
	if userID == "" {
		return nil, "missing user_id metadata"
	}

	if rawID := normalize.FirstNonEmpty(remote.Metadata["internal_price_id"], remote.Metadata["price_id"]); rawID != "" {
		priceID, parseErr := uuid.Parse(rawID)
		if parseErr == nil {
			price, err := prices.GetByID(ctx, priceID)
			if err == nil && price != nil {
				return &mappedSubscription{Remote: remote, UserID: userID, PriceID: price.ID}, ""
			}
		}
	}

	if stripePriceID := strings.TrimSpace(remote.StripePriceID); stripePriceID != "" {
		price, err := prices.GetByStripePriceID(ctx, stripePriceID)
		if err == nil && price != nil {
			return &mappedSubscription{Remote: remote, UserID: userID, PriceID: price.ID}, ""
		}
	}

	return nil, "no internal_price_id or mapped stripe price id"
}

// diffResult holds the keyed diff between remote and local subscription id sets.
type diffResult struct {
	RemoteOnly []string
	LocalOnly  []string
	Matched    int
}

// diffSubscriptions compares the set of remote and local subscription ids,
// mirroring subscription-sync's diffKeys behavior.
func diffSubscriptions(remoteIDs, localIDs map[string]struct{}) diffResult {
	result := diffResult{
		RemoteOnly: diffKeys(remoteIDs, localIDs),
		LocalOnly:  diffKeys(localIDs, remoteIDs),
	}
	for id := range remoteIDs {
		if _, ok := localIDs[id]; ok {
			result.Matched++
		}
	}
	return result
}

func diffKeys(a, b map[string]struct{}) []string {
	out := make([]string, 0)
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Run executes the Stripe backfill against the given runtime.
//
// It reconciles subscriptions (remote-active vs local-active) and, unless
// opts.SkipPayments, backfills charges into payments + card snapshots + the
// payment_methods table. Listers are injected so they can be faked in tests;
// chargeLister may be nil to skip the payments pass.
func Run(
	ctx context.Context,
	rt *app.Runtime,
	subLister subscriptions.StripeSubscriptionLister,
	chargeLister subscriptions.StripeChargeLister,
	opts Options,
) (*Report, error) {
	if rt == nil {
		return nil, fmt.Errorf("reconcile: runtime unavailable")
	}
	report := &Report{}

	if err := reconcileSubscriptions(ctx, rt, subLister, opts, report); err != nil {
		return report, err
	}

	if !opts.SkipPayments && chargeLister != nil {
		if err := reconcileCharges(ctx, rt, chargeLister, opts, report); err != nil {
			return report, err
		}
	}

	return report, nil
}

func reconcileSubscriptions(
	ctx context.Context,
	rt *app.Runtime,
	lister subscriptions.StripeSubscriptionLister,
	opts Options,
	report *Report,
) error {
	if lister == nil {
		return nil
	}
	fmt.Printf("\n== %s: subscriptions ==\n", stripeProcessorName)

	remoteSubs, err := lister.ListSubscriptions(ctx, opts.PageSize, opts.MaxPages)
	if err != nil {
		return fmt.Errorf("fetch stripe subscriptions failed: %w", err)
	}

	priceService := rt.PriceService
	if priceService == nil {
		priceService = catalog.NewPriceService(rt.DB)
	}

	remoteIDs := make(map[string]struct{})
	mapped := make(map[string]*mappedSubscription)
	remoteActiveCount := 0
	skipped := make([]string, 0)

	for _, remote := range remoteSubs {
		if remote.ID == "" {
			continue
		}
		if !subscriptions.StripeSubscriptionStatusActive(remote.Status) {
			continue
		}
		remoteActiveCount++
		if remote.CancelAtPeriodEnd {
			skipped = append(skipped, fmt.Sprintf("%s (cancel_at_period_end backfill unsupported)", remote.ID))
			continue
		}

		m, reason := mapRemoteSubscription(ctx, priceService, remote)
		if reason != "" {
			skipped = append(skipped, fmt.Sprintf("%s (%s)", remote.ID, reason))
			continue
		}
		known, err := opts.userKnown(ctx, m.UserID)
		if err != nil {
			return fmt.Errorf("user existence check failed for %s: %w", remote.ID, err)
		}
		if !known {
			report.UnknownUserSubscriptions = append(report.UnknownUserSubscriptions,
				fmt.Sprintf("%s (user %s not found locally)", remote.ID, m.UserID))
			continue
		}
		remoteIDs[remote.ID] = struct{}{}
		mapped[remote.ID] = m
	}

	subService := subscriptions.NewSubscriptionService(rt.DB, nil, nil, nil, nil, nil)
	localSubs, err := subService.GetActiveSubscriptionsByProcessor(ctx, stripeProcessorName)
	if err != nil {
		return fmt.Errorf("fetch local subscriptions failed: %w", err)
	}

	localIDs := make(map[string]struct{}, len(localSubs))
	for _, sub := range localSubs {
		id := strings.TrimSpace(sub.ProcessorSubscriptionID)
		if id != "" {
			localIDs[id] = struct{}{}
		}
	}

	diff := diffSubscriptions(remoteIDs, localIDs)

	report.RemoteSubscriptionsFetched = len(remoteSubs)
	report.RemoteActiveSubscriptions = remoteActiveCount
	report.MappableSubscriptions = len(remoteIDs)
	report.SkippedSubscriptions = skipped
	report.LocalActiveSubscriptions = len(localSubs)
	report.RemoteOnlySubscriptions = diff.RemoteOnly
	report.LocalOnlySubscriptions = diff.LocalOnly
	report.MatchedSubscriptions = diff.Matched

	fmt.Printf("remote subscriptions fetched: %d (active=%d, mappable=%d, skipped=%d)\n", len(remoteSubs), remoteActiveCount, len(remoteIDs), len(skipped))
	fmt.Printf("local active subscriptions: %d (unique ids=%d)\n", len(localSubs), len(localIDs))
	fmt.Printf("matched subscriptions: %d\n", diff.Matched)

	if len(skipped) == 0 {
		fmt.Println("skipped (unmappable) subscriptions: none")
	} else {
		fmt.Printf("skipped (unmappable) subscriptions (%d):\n", len(skipped))
		for _, entry := range skipped {
			fmt.Printf("  %s\n", entry)
		}
	}

	// Loud: an active Stripe subscription bound to a user we have no local record
	// of is a red flag (possibly billing a deleted account). We never create
	// billing rows for unknown users.
	if len(report.UnknownUserSubscriptions) > 0 {
		fmt.Printf("!! SKIPPED - unknown user (%d subscription(s); no local account, billing NOT created):\n", len(report.UnknownUserSubscriptions))
		for _, entry := range report.UnknownUserSubscriptions {
			fmt.Printf("  %s\n", entry)
		}
	}

	if len(diff.RemoteOnly) == 0 {
		fmt.Println("remote-only subscriptions: none")
	} else {
		fmt.Printf("remote-only subscriptions (%d):\n", len(diff.RemoteOnly))
		for _, id := range diff.RemoteOnly {
			fmt.Printf("  %s\n", id)
		}
	}

	if len(diff.LocalOnly) == 0 {
		fmt.Println("local-only subscriptions: none")
	} else {
		fmt.Printf("local-only subscriptions (%d) — flagged only, not cancelled:\n", len(diff.LocalOnly))
		for _, id := range diff.LocalOnly {
			fmt.Printf("  %s\n", id)
		}
	}

	if !opts.Apply {
		if len(diff.RemoteOnly) > 0 {
			fmt.Printf("\ndry-run: would create %d local membership(s); pass --apply to write\n", len(diff.RemoteOnly))
		}
		return nil
	}

	if len(diff.RemoteOnly) == 0 {
		return nil
	}

	// Safety guard: refuse a large/destructive apply when the remote report
	// returned zero subscriptions while local active subs exist (mirrors the
	// NMI guard in subscription-sync). A zero-length remote report most likely
	// indicates an auth/API problem rather than a genuinely empty account.
	if len(remoteSubs) == 0 && len(localIDs) > 0 {
		return fmt.Errorf("refusing to apply stripe reconciliation: remote report returned zero subscriptions while %d local active subscriptions exist", len(localIDs))
	}

	if rt.SubscriptionLifecycleService == nil {
		return fmt.Errorf("subscription lifecycle service unavailable; cannot apply changes")
	}

	created := 0
	for _, id := range diff.RemoteOnly {
		m, ok := mapped[id]
		if !ok {
			fmt.Printf("skip %s: missing mapping\n", id)
			continue
		}
		subID := id
		_, err := rt.SubscriptionLifecycleService.CreateMembership(ctx, &subscriptions.CreateMembershipParams{
			UserID:                  m.UserID,
			PriceID:                 m.PriceID,
			Processor:               models.ProcessorStripe,
			ProcessorSubscriptionID: &subID,
			CurrentPeriodStartsAt:   zeroTimeNil(m.Remote.CurrentPeriodStart),
			CurrentPeriodEndsAt:     zeroTimeNil(m.Remote.CurrentPeriodEnd),
		})
		if err != nil {
			fmt.Printf("failed to create membership for %s: %v\n", id, err)
			continue
		}
		created++
		fmt.Printf("created local membership for remote subscription %s (user=%s price=%s)\n", id, m.UserID, m.PriceID)
	}
	report.MembershipsCreated = created
	fmt.Printf("created %d local membership(s)\n", created)

	return nil
}

func reconcileCharges(
	ctx context.Context,
	rt *app.Runtime,
	lister subscriptions.StripeChargeLister,
	opts Options,
	report *Report,
) error {
	fmt.Printf("\n== %s: payments + cards ==\n", stripeProcessorName)

	charges, err := lister.ListCharges(ctx, opts.PageSize, opts.MaxPages)
	if err != nil {
		return fmt.Errorf("fetch stripe charges failed: %w", err)
	}
	report.RemoteChargesFetched = len(charges)

	paymentService := rt.PaymentService
	subService := rt.SubscriptionService
	customers := rt.ProcessorCustomerService
	priceService := rt.PriceService
	if priceService == nil {
		priceService = catalog.NewPriceService(rt.DB)
	}
	if paymentService == nil {
		paymentService = payments.NewPaymentService(rt.DB, rt.Clock)
	}
	if subService == nil {
		subService = subscriptions.NewSubscriptionService(rt.DB, nil, nil, nil, nil, nil)
	}
	if customers == nil {
		customers = payments.NewProcessorCustomerService(rt.DB)
	}

	var cardLinker payments.StripeCardSubscriptionLinker
	if subService != nil {
		cardLinker = subService
	}

	skipped := make([]string, 0)
	mapped := 0
	paymentsCreated := 0
	cardsSnapshot := 0
	cardsUpserted := 0

	for _, charge := range charges {
		txnID := normalize.FirstNonEmpty(charge.ID, charge.PaymentIntent)
		if txnID == "" {
			continue
		}
		// Only paid charges represent money we should mirror.
		if !charge.Paid && strings.TrimSpace(charge.Status) != "succeeded" {
			continue
		}

		// Resolve the local user + subscription this charge belongs to.
		userID, sub := resolveChargeOwner(ctx, subService, customers, charge)
		if userID == "" {
			skipped = append(skipped, fmt.Sprintf("%s (no mapped user)", txnID))
			continue
		}
		known, err := opts.userKnown(ctx, userID)
		if err != nil {
			return fmt.Errorf("user existence check failed for charge %s: %w", txnID, err)
		}
		if !known {
			report.UnknownUserCharges = append(report.UnknownUserCharges,
				fmt.Sprintf("%s (user %s not found locally)", txnID, userID))
			continue
		}
		mapped++

		card := payments.NormalizeStripeCard(charge.Card)

		if !opts.Apply {
			continue
		}

		// Ensure a payment row exists (idempotent: create-if-not-exists keyed on
		// processor + transaction_id). We insert directly so the backfill never
		// re-runs side effects (credits/entitlements) that RegisterPurchase would.
		createdPayment, err := ensureChargePayment(ctx, paymentService, priceService, userID, sub, charge, txnID, card)
		if err != nil {
			fmt.Printf("failed to ensure payment for %s: %v\n", txnID, err)
			continue
		}
		if createdPayment {
			paymentsCreated++
		}

		if card != nil {
			// Snapshot card onto the payment row(s). Preview invoice.paid rows can be
			// keyed by invoice id, so include it alongside charge/payment_intent.
			snapTxns := make([]string, 0, 3)
			if id := strings.TrimSpace(charge.ID); id != "" {
				snapTxns = append(snapTxns, id)
			}
			if pi := strings.TrimSpace(charge.PaymentIntent); pi != "" {
				snapTxns = append(snapTxns, pi)
			}
			if invoiceID := strings.TrimSpace(charge.Invoice); invoiceID != "" {
				snapTxns = append(snapTxns, invoiceID)
			}
			if err := payments.SnapshotPaymentCard(ctx, rt.DB, snapTxns, card); err != nil {
				fmt.Printf("failed to snapshot card for %s: %v\n", txnID, err)
			} else {
				cardsSnapshot++
			}

			// Upsert the payment_methods row + link the active subscription.
			if err := payments.UpsertStripeCardForCustomer(ctx, rt.DB, customers, cardLinker, rt.Clock, charge.CustomerID, charge.PaymentMethod, charge.ID, card); err != nil {
				fmt.Printf("failed to upsert card for %s: %v\n", txnID, err)
			} else {
				cardsUpserted++
			}
		}
	}

	report.ChargesMapped = mapped
	report.ChargesSkipped = skipped
	report.PaymentsCreated = paymentsCreated
	report.PaymentCardsSnapshot = cardsSnapshot
	report.CardsUpserted = cardsUpserted

	fmt.Printf("remote charges fetched: %d (mapped=%d, skipped=%d)\n", len(charges), mapped, len(skipped))
	if len(report.UnknownUserCharges) > 0 {
		fmt.Printf("!! SKIPPED - unknown user (%d charge(s); no local account, payment/card NOT created):\n", len(report.UnknownUserCharges))
		for _, entry := range report.UnknownUserCharges {
			fmt.Printf("  %s\n", entry)
		}
	}
	if !opts.Apply {
		if mapped > 0 {
			fmt.Printf("dry-run: would backfill payments/cards for %d charge(s); pass --apply to write\n", mapped)
		}
		return nil
	}
	fmt.Printf("payments created: %d; payment cards snapshotted: %d; payment methods upserted: %d\n", paymentsCreated, cardsSnapshot, cardsUpserted)
	return nil
}

// resolveChargeOwner finds the local user (and optional subscription) for a
// charge. It prefers the subscription tied to the charge's invoice/subscription,
// then falls back to the processor-customer mapping. Read-only.
func resolveChargeOwner(
	ctx context.Context,
	subService *subscriptions.SubscriptionService,
	customers *payments.ProcessorCustomerService,
	charge subscriptions.StripeRemoteCharge,
) (string, *models.Subscription) {
	if customers != nil {
		if cid := strings.TrimSpace(charge.CustomerID); cid != "" {
			if uid, err := customers.GetUserIDByCustomerID(ctx, string(models.ProcessorStripe), cid); err == nil && strings.TrimSpace(uid) != "" {
				userID := strings.TrimSpace(uid)
				var sub *models.Subscription
				if subService != nil {
					if active, err := subService.GetActiveSubscription(ctx, userID); err == nil {
						sub = active
					}
				}
				return userID, sub
			}
		}
	}
	return "", nil
}

// ensureChargePayment inserts a openrails.payments row for a charge if one does
// not already exist for (stripe, transaction_id). Returns true when a new row
// was created. The card snapshot is set on insert; SnapshotPaymentCard later
// fills any sibling rows (e.g. matched by payment_intent).
func ensureChargePayment(
	ctx context.Context,
	paymentService *payments.PaymentService,
	priceService priceResolver,
	userID string,
	sub *models.Subscription,
	charge subscriptions.StripeRemoteCharge,
	txnID string,
	card *payments.StripeCard,
) (bool, error) {
	// Skip if a payment already exists for this charge id, payment_intent, or
	// invoice id. Preview invoice.paid can create the row before charge backfill.
	for _, candidate := range []string{strings.TrimSpace(charge.ID), strings.TrimSpace(charge.PaymentIntent), strings.TrimSpace(charge.Invoice)} {
		if candidate == "" {
			continue
		}
		if existing, err := paymentService.GetByTransactionID(ctx, models.ProcessorStripe, candidate); err == nil && existing != nil {
			return false, nil
		}
	}

	// A payment row requires a price_id (NOT NULL). Derive it from the
	// subscription when present; without one we cannot safely create the row, so
	// only the card snapshot/upsert paths apply for this charge.
	var priceID uuid.UUID
	if sub != nil && sub.PriceID != uuid.Nil {
		priceID = sub.PriceID
	}
	if priceID == uuid.Nil {
		return false, nil
	}

	currency := strings.TrimSpace(charge.Currency)
	if currency == "" {
		currency = "usd"
	}

	now := paymentService.Clock().Now()
	payment := &models.Payment{
		ID:              uuidutil.NewV7(),
		TenantSubjectID: identity.TenantSubjectIDFromString(userID).UUID(),
		PriceID:         priceID,
		Processor:       models.ProcessorStripe,
		TransactionID:   txnID,
		Amount:          charge.Amount,
		ListAmount:      charge.Amount,
		Currency:        currency,
		Status:          payments.PaymentStatusCompletedValue,
		PurchasedAt:     now,
		CreatedAt:       now,
		Metadata: map[string]any{
			"source":            "stripe_reconcile_backfill",
			"stripe_charge_id":  strings.TrimSpace(charge.ID),
			"stripe_invoice_id": strings.TrimSpace(charge.Invoice),
		},
	}
	if sub != nil {
		id := sub.ID
		payment.SubscriptionID = &id
	}
	if card != nil {
		brand := card.Brand
		last4 := card.Last4
		payment.CardBrand = &brand
		payment.CardLast4 = &last4
	}

	// CreateIfNotExists is keyed on (processor, transaction_id) via the unique
	// constraint, so a concurrent or repeat run is a no-op.
	return paymentService.CreateIfNotExists(ctx, payment)
}

func zeroTimeNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
