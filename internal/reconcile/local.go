package reconcile

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// localRailNames maps a reconcile Provider onto the rail name(s)
// used by local billing rows. NMI subscriptions historically carry either
// "mobius" (the branded gateway) or "nmi".
func localRailNames(p Provider) []string {
	switch p {
	case ProviderNMI:
		return []string{"nmi", "mobius"}
	case ProviderCCBill:
		return []string{"ccbill"}
	case ProviderStripe:
		return []string{"stripe"}
	case ProviderSolana:
		return []string{"solana"}
	default:
		return []string{string(p)}
	}
}

// LocalSubscription is the slice of openrails.subscriptions the diff engine
// consumes.
type LocalSubscription struct {
	ID                    uuid.UUID
	CustomerID            uuid.UUID
	PriceID               *uuid.UUID
	ProductID             uuid.UUID
	Status                string
	Rail                  string
	RailSubscriptionID    string
	UserEmail             string
	PaymentMethodID       *uuid.UUID
	CurrentPeriodStartsAt *time.Time
	CurrentPeriodEndsAt   *time.Time
	StartedAt             time.Time
	EndedAt               *time.Time
	CancelledAt           *time.Time
	CancelType            string
	DeletionScheduledAt   *time.Time
	TierGroup             string
	LastRetryAt           *time.Time
	RetryAttempts         int
	NextRetryAt           *time.Time
	// EntitlementNames are the keys of entitlements_spec_snapshot — the
	// entitlements this subscription is supposed to grant.
	EntitlementNames []string
}

// IsLive reports whether the subscription occupies a "billing relationship
// exists" state locally.
func (s *LocalSubscription) IsLive() bool {
	switch s.Status {
	case "active", "past_due", "pending":
		return true
	}
	return false
}

// LocalPayment is the slice of openrails.payments the diff engine consumes.
type LocalPayment struct {
	ID                uuid.UUID
	CustomerID        uuid.UUID
	Rail              string
	TransactionID     string
	AmountCents       int64
	Status            string
	SubscriptionID    *uuid.UUID
	RefundedPaymentID *uuid.UUID
	PurchasedAt       time.Time
}

// LocalEntitlement is one LIVE subscription-sourced entitlement window.
type LocalEntitlement struct {
	ID          uuid.UUID
	CustomerID  uuid.UUID
	Entitlement string
	SourceID    uuid.UUID
	StartAt     time.Time
	EndAt       *time.Time
}

// LocalPaymentMethod is the slice of openrails.payment_methods the diff engine
// consumes.
type LocalPaymentMethod struct {
	ID         uuid.UUID
	CustomerID uuid.UUID
	Rail       string
	VaultID    string
	LastFour   string
	CardType   string
	ExpiryDate string
}

// LocalPrice is the slice of openrails.prices the PS-1 materializer consumes:
// the catalog provider_links (rails jsonb) map remote plan ids onto
// local prices.
type LocalPrice struct {
	ID               uuid.UUID
	ProductID        uuid.UUID
	Amount           int64
	Currency         string
	BillingCycleDays *int
	Status           string
	// Rails is the provider-links blob: rail name -> config map
	// (plan_id / price_id / ...), as written by catalog bootstrap.
	Rails map[string]map[string]string
}

// LocalState is one provider's local billing state, loaded merchant-scoped.
type LocalState struct {
	Subscriptions  []LocalSubscription
	Entitlements   []LocalEntitlement
	PaymentMethods []LocalPaymentMethod
	// Prices are the billable prices carrying provider links (all providers;
	// the diff filters by the provider's local rail names).
	Prices []LocalPrice
}

// LocalStateLoader loads the local rows the diff engine compares against a
// provider snapshot. PaymentsByTransactionIDs is queried separately (bounded
// by the snapshot's transaction set rather than a date window, so clock skew
// between us and the rail can not fake a missing payment).
type LocalStateLoader interface {
	Load(ctx context.Context, provider Provider, providerAccountID *uuid.UUID) (*LocalState, error)
	PaymentsByTransactionIDs(ctx context.Context, provider Provider, providerAccountID *uuid.UUID, transactionIDs []string) ([]LocalPayment, error)
}

// PGLocalStateLoader loads local state through the sqlc layer on a
// merchant-pinned connection.
type PGLocalStateLoader struct {
	DB *db.DB
}

var _ LocalStateLoader = (*PGLocalStateLoader)(nil)

func (l *PGLocalStateLoader) Load(ctx context.Context, provider Provider, providerAccountID *uuid.UUID) (*LocalState, error) {
	names := localRailNames(provider)
	q := l.DB.Gen(ctx)

	state := &LocalState{}

	subs, err := q.ReconcileListSubscriptionsByRails(ctx, gen.ReconcileListSubscriptionsByRailsParams{
		Rails:             names,
		ProviderAccountID: providerAccountID,
	})
	if err != nil {
		return nil, err
	}
	for _, row := range subs {
		s := LocalSubscription{
			ID:                    row.ID,
			CustomerID:            row.CustomerID,
			PriceID:               row.PriceID,
			ProductID:             row.ProductID,
			Status:                string(row.Status),
			Rail:                  row.Rail,
			RailSubscriptionID:    row.RailSubscriptionID,
			PaymentMethodID:       row.PaymentMethodID,
			CurrentPeriodStartsAt: row.CurrentPeriodStartsAt,
			CurrentPeriodEndsAt:   row.CurrentPeriodEndsAt,
			StartedAt:             row.StartedAt,
			EndedAt:               row.EndedAt,
			CancelledAt:           row.CancelledAt,
			DeletionScheduledAt:   row.DeletionScheduledAt,
			LastRetryAt:           row.LastRetryAt,
			NextRetryAt:           row.NextRetryAt,
		}
		if row.UserEmail != nil {
			s.UserEmail = *row.UserEmail
		}
		if row.CancelType != nil {
			s.CancelType = *row.CancelType
		}
		if row.TierGroup != nil {
			s.TierGroup = *row.TierGroup
		}
		if row.RetryAttempts != nil {
			s.RetryAttempts = int(*row.RetryAttempts)
		}
		if len(row.EntitlementsSpecSnapshot) > 0 {
			var spec map[string]json.RawMessage
			if err := json.Unmarshal(row.EntitlementsSpecSnapshot, &spec); err == nil {
				for name := range spec {
					s.EntitlementNames = append(s.EntitlementNames, name)
				}
			}
		}
		state.Subscriptions = append(state.Subscriptions, s)
	}

	ents, err := q.ReconcileListSubscriptionEntitlements(ctx, gen.ReconcileListSubscriptionEntitlementsParams{
		Rails:             names,
		ProviderAccountID: providerAccountID,
	})
	if err != nil {
		return nil, err
	}
	for _, row := range ents {
		state.Entitlements = append(state.Entitlements, LocalEntitlement{
			ID:          row.ID,
			CustomerID:  row.CustomerID,
			Entitlement: row.Entitlement,
			SourceID:    row.SourceID,
			StartAt:     row.StartAt,
			EndAt:       row.EndAt,
		})
	}

	prices, err := q.ReconcileListPricesWithRails(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range prices {
		p := LocalPrice{
			ID:        row.ID,
			ProductID: row.ProductID,
			Amount:    row.Amount,
			Currency:  row.Currency,
			Status:    row.Status,
		}
		// Only an auto-renewing price has a recurring cadence to match a remote
		// provider plan against (#622).
		if row.AutoRenew && row.AccessDurationDays != nil {
			days := int(*row.AccessDurationDays)
			p.BillingCycleDays = &days
		}
		if len(row.Rails) > 0 {
			// Tolerate malformed blobs: a price whose links can't decode simply
			// never matches a remote plan (PS-1 stays requires_review).
			_ = json.Unmarshal(row.Rails, &p.Rails)
		}
		state.Prices = append(state.Prices, p)
	}

	pms, err := q.ReconcileListPaymentMethodsByRails(ctx, gen.ReconcileListPaymentMethodsByRailsParams{
		Rails:             names,
		ProviderAccountID: providerAccountID,
	})
	if err != nil {
		return nil, err
	}
	for _, row := range pms {
		pm := LocalPaymentMethod{
			ID:         row.ID,
			CustomerID: row.CustomerID,
			Rail:       row.Rail,
			VaultID:    row.VaultID,
		}
		if row.LastFour != nil {
			pm.LastFour = *row.LastFour
		}
		if row.CardType != nil {
			pm.CardType = *row.CardType
		}
		if row.ExpiryDate != nil {
			pm.ExpiryDate = *row.ExpiryDate
		}
		state.PaymentMethods = append(state.PaymentMethods, pm)
	}

	return state, nil
}

func (l *PGLocalStateLoader) PaymentsByTransactionIDs(ctx context.Context, provider Provider, providerAccountID *uuid.UUID, transactionIDs []string) ([]LocalPayment, error) {
	if len(transactionIDs) == 0 {
		return nil, nil
	}
	rows, err := l.DB.Gen(ctx).ReconcileListPaymentsByTransactionIDs(ctx, gen.ReconcileListPaymentsByTransactionIDsParams{
		Rails:             localRailNames(provider),
		ProviderAccountID: providerAccountID,
		TransactionIds:    transactionIDs,
	})
	if err != nil {
		return nil, err
	}
	out := make([]LocalPayment, 0, len(rows))
	for _, row := range rows {
		p := LocalPayment{
			ID:                row.ID,
			CustomerID:        row.CustomerID,
			Rail:              string(row.Rail),
			TransactionID:     row.TransactionID,
			AmountCents:       row.Amount / moneyutil.MicrosPerCent,
			Status:            string(row.Status),
			SubscriptionID:    row.SubscriptionID,
			RefundedPaymentID: row.RefundedPaymentID,
			PurchasedAt:       row.PurchasedAt,
		}
		out = append(out, p)
	}
	return out, nil
}

// SolanaSubscriptionSourceFromDB adapts openrails.solana_subscriptions into the
// SolanaFetcher's subscription source (one-line phase-2 wiring promised by
// the phase-1 design).
func SolanaSubscriptionSourceFromDB(d *db.DB) SolanaSubscriptionSource {
	return func(ctx context.Context) ([]SolanaSubscriptionRef, error) {
		rows, err := d.Gen(ctx).ReconcileListSolanaSubscriptionRefs(ctx)
		if err != nil {
			return nil, err
		}
		refs := make([]SolanaSubscriptionRef, 0, len(rows))
		for _, row := range rows {
			refs = append(refs, SolanaSubscriptionRef{
				SubscriptionPDA:  row.SubscriptionPda,
				PlanPDA:          row.PlanPda,
				SubscriberWallet: row.SubscriberWallet,
			})
		}
		return refs, nil
	}
}
