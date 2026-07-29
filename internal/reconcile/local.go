package reconcile

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// localRailNames maps a reconcile Provider onto the rail name(s)
// used by local billing rows. NMI subscriptions historically carry either
// the gateway rail "nmi".
func localRailNames(p Provider) []string {
	switch p {
	case ProviderNMI:
		return []string{string(models.RailNMI)}
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
// the catalog psp_links jsonb maps remote plan ids onto
// local prices.
type LocalPrice struct {
	ID               uuid.UUID
	ProductID        uuid.UUID
	Amount           int64
	Currency         string
	BillingCycleDays *int
	Archived         bool
	// PSPLinks is the provider-links blob: PSP key -> link entry
	// (rail / plan_id / price_id / ...), as written by catalog apply.
	PSPLinks map[string]map[string]string
}

// LocalState is one provider's local billing state, loaded merchant-scoped.
type LocalState struct {
	Subscriptions  []LocalSubscription
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
		Rails: names,
		PspID: providerAccountID,
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

	prices, err := q.ReconcileListPricesWithPSPLinks(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range prices {
		p := LocalPrice{
			ID:        row.ID,
			ProductID: row.ProductID,
			Amount:    row.Amount,
			Currency:  row.Currency,
			Archived:  row.Archived,
		}
		// Only an auto-renewing price has a recurring cadence to match a remote
		// provider plan against (#622). The window is in hours; the provider
		// cadence is whole days (hours/24).
		if row.AutoRenew && row.AccessDurationHours != nil {
			days := int(*row.AccessDurationHours) / 24
			p.BillingCycleDays = &days
		}
		if len(row.PspLinks) > 0 {
			// Tolerate malformed blobs: a price whose links can't decode simply
			// never matches a remote plan (PS-1 stays requires_review).
			_ = json.Unmarshal(row.PspLinks, &p.PSPLinks)
		}
		state.Prices = append(state.Prices, p)
	}

	pms, err := q.ReconcileListPaymentMethodsByRails(ctx, gen.ReconcileListPaymentMethodsByRailsParams{
		Rails: names,
		PspID: providerAccountID,
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
		Rails:          localRailNames(provider),
		PspID:          providerAccountID,
		TransactionIds: transactionIDs,
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

// SolanaPlanSourceFromDB lists OUR plan PDAs for the #714 enumeration: the
// union of locally-known subscription rows and the catalog's
// psp_links["solana"].plan_pda provider links (so a fresh DB can still enumerate
// from catalog alone).
func SolanaPlanSourceFromDB(d *db.DB) SolanaPlanSource {
	return func(ctx context.Context) ([]string, error) {
		set := map[string]struct{}{}
		refs, err := d.Gen(ctx).ReconcileListSolanaSubscriptionRefs(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range refs {
			if pda := strings.TrimSpace(r.PlanPda); pda != "" {
				set[pda] = struct{}{}
			}
		}
		prices, err := d.Gen(ctx).ReconcileListPricesWithPSPLinks(ctx)
		if err != nil {
			return nil, err
		}
		for _, row := range prices {
			if len(row.PspLinks) == 0 {
				continue
			}
			var rails map[string]map[string]string
			if err := json.Unmarshal(row.PspLinks, &rails); err != nil {
				continue // malformed links never match; same tolerance as Load
			}
			if pda := strings.TrimSpace(rails["solana"]["plan_pda"]); pda != "" {
				set[pda] = struct{}{}
			}
		}
		out := make([]string, 0, len(set))
		for pda := range set {
			out = append(out, pda)
		}
		sort.Strings(out)
		return out, nil
	}
}

// SolanaDueSubscriptionSourceFromDB adapts the existing ListDueSolanaSubscriptions
// query into the #720 due-window source: subscription_pda values whose
// next_pull_at is at/before `before`. That query already filters
// server-side (status='active' AND next_pull_at<=$1), so this read is
// due-proportional, not O(all subs) — unlike SolanaSubscriptionSourceFromDB
// above, which stays exhaustive on purpose (narrowed probes and the #714
// discovery de-dup set both need every locally-known ref).
func SolanaDueSubscriptionSourceFromDB(d *db.DB) SolanaDueSubscriptionSource {
	return func(ctx context.Context, before time.Time) (map[string]struct{}, error) {
		rows, err := d.Gen(ctx).ListDueSolanaSubscriptions(ctx, gen.ListDueSolanaSubscriptionsParams{
			Now: before.UTC(),
		})
		if err != nil {
			return nil, err
		}
		out := make(map[string]struct{}, len(rows))
		for _, r := range rows {
			out[r.SubscriptionPda] = struct{}{}
		}
		return out, nil
	}
}

// SolanaLocalRecordResolverFromDB resolves #713 memo local-ids against the two
// record kinds the stamp names: checkout sessions (one-off local-id = session
// id) and rail intents (pull local-id = #674 intent id). (nil, nil) = no local
// record; backend errors surface so the run retries instead of parking noise.
func SolanaLocalRecordResolverFromDB(d *db.DB) SolanaLocalRecordResolver {
	return func(ctx context.Context, localID uuid.UUID) (*SolanaLocalRecord, error) {
		row, err := d.Gen(ctx).GetCheckoutSessionByID(ctx, localID)
		if err == nil {
			session, err := models.CheckoutSessionFromGen(row)
			if err != nil {
				return nil, err
			}
			rec := &SolanaLocalRecord{
				Kind:                SolanaLocalKindCheckoutSession,
				Rail:                string(session.Rail),
				CustomerID:          session.CustomerID,
				PriceID:             session.PriceID,
				SessionStatus:       string(session.Status),
				ExpectedRecipient:   solanaStateStr(session.RailState, "recipient"),
				ExpectedMint:        solanaStateStr(session.RailState, "token_mint"),
				ExpectedTokenAmount: solanaStateU64(session.RailState, "token_amount"),
			}
			if session.TransactionID != nil {
				rec.SettledTransactionID = strings.TrimSpace(*session.TransactionID)
			}
			return rec, nil
		}
		if !db.IsNotFound(err) {
			return nil, err
		}
		intent, err := d.Gen(ctx).GetRailIntent(ctx, localID)
		if err == nil {
			rec := &SolanaLocalRecord{Kind: SolanaLocalKindPullIntent, Rail: intent.Rail}
			var payload struct {
				SubscriptionPDA string `json:"subscription_pda"`
			}
			if len(intent.Payload) > 0 {
				_ = json.Unmarshal(intent.Payload, &payload)
			}
			rec.SubscriptionPDA = strings.TrimSpace(payload.SubscriptionPDA)
			return rec, nil
		}
		if !db.IsNotFound(err) {
			return nil, err
		}
		return nil, nil
	}
}

// solanaStateStr / solanaStateU64 read the checkout session's rail_state jsonb
// (written by the solana checkout flow: recipient / token_mint / token_amount).
func solanaStateStr(state map[string]any, key string) string {
	if s, ok := state[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func solanaStateU64(state map[string]any, key string) uint64 {
	// or#863: NO float64 case. The canonical JSONB shape for a base-unit amount
	// is a decimal string (session_service writes strconv.FormatUint), and a
	// token amount that arrived as a JSON number has already been through a
	// float64 — it cannot be trusted to equal the on-chain transfer it is about
	// to be compared against. An unreadable amount yields 0, which parks the
	// settlement ("carries no bound solana token quote to verify against")
	// rather than approving a transfer against a rounded expectation.
	switch v := state[key].(type) {
	case string:
		if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
			return n
		}
	case json.Number:
		if n, err := strconv.ParseUint(v.String(), 10, 64); err == nil {
			return n
		}
	}
	return 0
}
