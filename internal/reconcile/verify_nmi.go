package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// nmiReader is the merchant's armed NMI client and the PSP it reads.
func nmiReader(armed MerchantPullClients) (*nmi.NMIClient, uuid.UUID) {
	p, ok := armed.Probers[ProviderNMI].(*NMISubscriptionProber)
	if !ok || p.Client == nil {
		return nil, uuid.Nil
	}
	return p.Client, armed.Coverage[ProviderNMI].Binding.ID
}

// resolveNMIBatch reads unverified NMI schedules nmi.MaxQueryIDs at a time:
// one recurring report and one transaction query per batch, plus a v5 read
// only for a schedule the report no longer lists. Each row is then decided
// and applied from its own evidence.
func resolveNMIBatch(ctx context.Context, database *db.DB, lc *subscriptions.SubscriptionLifecycleService, client *nmi.NMIClient, subs []*models.Subscription, now time.Time) error {
	var errs error
	for len(subs) > 0 {
		n := min(len(subs), nmi.MaxQueryIDs)
		errs = errors.Join(errs, resolveNMIChunk(ctx, database, lc, client, subs[:n], now))
		subs = subs[n:]
	}
	return errs
}

func resolveNMIChunk(ctx context.Context, database *db.DB, lc *subscriptions.SubscriptionLifecycleService, client *nmi.NMIClient, subs []*models.Subscription, now time.Time) error {
	ids := make([]string, 0, len(subs))
	var since time.Time
	for _, s := range subs {
		ids = append(ids, s.RailSubscriptionID)
		if s.CurrentPeriodEndsAt != nil {
			at := s.CurrentPeriodEndsAt.UTC().Add(-AlignmentSlack(s.CurrentPeriodStartsAt, s.CurrentPeriodEndsAt))
			if since.IsZero() || at.Before(since) {
				since = at
			}
		}
	}
	records, err := client.ReadSchedules(ctx, ids)
	if err != nil {
		return fmt.Errorf("nmi recurring report: %w", err)
	}
	var sales []nmi.ScheduleSale
	if !since.IsZero() {
		if sales, err = client.SalesForSchedules(ctx, ids, since); err != nil {
			return fmt.Errorf("nmi transaction query: %w", err)
		}
	}
	vaults, err := vaultsOf(ctx, database, subs)
	if err != nil {
		return err
	}
	a := newAttribution(subs, vaults, records)
	for _, sale := range sales {
		a.add(sale)
	}

	snaps := make(map[uuid.UUID]*RemoteSnapshot, len(subs))
	var errs error
	for _, sub := range subs {
		if a.ambiguous[sub.ID] {
			// Another schedule in the batch shares the vault: read this one
			// alone, by its own schedule id.
			snap, err := (&NMISubscriptionProber{Client: client}).ProbeSubscription(ctx, ProbeSubject{LocalID: sub.ID, RailSubscriptionID: sub.RailSubscriptionID,
				PeriodStart: sub.CurrentPeriodStartsAt, PeriodEnd: sub.CurrentPeriodEndsAt, ObservedAt: now})
			if err != nil {
				errs = errors.Join(errs, err)
				continue
			}
			snaps[sub.ID] = snap
			continue
		}
		snap := &RemoteSnapshot{Provider: ProviderNMI, FetchedAt: now, Transactions: a.txns[sub.ID],
			Coverage:     SnapshotCoverage{SubscriptionsExhaustive: true},
			Capabilities: Capabilities{Subscriptions: true, Transactions: !since.IsZero()}}
		if rec, ok := records[sub.RailSubscriptionID]; ok {
			snap.Subscriptions = []RemoteSubscription{nmiRosterEntry(sub.RailSubscriptionID, rec.NextChargeDate, now)}
		} else {
			// Absent from the report: the v5 read is authoritative (404 or a
			// deletion tombstone means NMI ended the schedule).
			live, err := client.GetRecurringLiveness(ctx, sub.RailSubscriptionID)
			if err != nil {
				errs = errors.Join(errs, err)
				continue
			}
			if live.Found {
				snap.Subscriptions = []RemoteSubscription{nmiRosterEntry(sub.RailSubscriptionID, live.NextChargeDate, now)}
			}
		}
		snaps[sub.ID] = snap
	}
	return errors.Join(errs, applyVerified(ctx, database, lc, subs, snaps, now))
}

// applyVerified decides every read row, holds all of the batch's
// cancellations when they exceed the pass budget (#837), and applies the rest.
func applyVerified(ctx context.Context, database *db.DB, lc *subscriptions.SubscriptionLifecycleService, subs []*models.Subscription, snaps map[uuid.UUID]*RemoteSnapshot, now time.Time) error {
	if len(snaps) == 0 {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	floor := EvidenceFloorFor(ctx, database, mid.UUID())
	q := database.Gen(ctx)
	cancels := 0
	for _, sub := range subs {
		if snap := snaps[sub.ID]; snap != nil {
			d := Decide(SubscriptionStateOf(sub), EvidenceBundle{Snapshot: snap, EvidenceFloor: floor}, now, 0)
			if d.Kind == TransitionCancel {
				cancels++
			}
		}
	}
	holdCancels := false
	if cancels > 0 {
		// The kill switch and first-enforce gate (#835/#836) hold every
		// destructive outcome of a read, as they do the refresh pass's.
		if verdict := destructive.New(database).Check(ctx, mid.UUID()); !verdict.Allowed || !verdict.EnforceArmed {
			holdCancels = true
		}
	}
	if cancels > 0 && !holdCancels {
		live, err := q.CountLiveLinkedSubscriptionsForRail(ctx, gen.CountLiveLinkedSubscriptionsForRailParams{MerchantID: mid.UUID(), Rail: string(models.RailNMI)})
		if err != nil {
			return fmt.Errorf("verify: count live nmi book: %w", err)
		}
		if exceeded, reason := (CancelBudget{}).Exceeded(cancels, int(live)); exceeded {
			holdCancels = true
			recordGuardFinding(ctx, q, mid, ProviderNMI, "cancellation_cap", reason)
		}
	}
	var errs error
	for _, sub := range subs {
		snap := snaps[sub.ID]
		if snap == nil {
			continue
		}
		if holdCancels && Decide(SubscriptionStateOf(sub), EvidenceBundle{Snapshot: snap, EvidenceFloor: floor}, now, 0).Kind == TransitionCancel {
			continue
		}
		res, err := ConvergeSubscriptionFromSnapshot(ctx, database, lc, sub, snap, now, 0)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("verify %s: %w", sub.ID, err))
			continue
		}
		log.WithContext(ctx).WithFields(log.Fields{"subscription_id": sub.ID, "transition": res.Decision.Kind.String(), "reason": res.Decision.Reason, "applied": res.Applied}).
			Debug("verify: decided from the provider read")
	}
	return errs
}

func nmiRosterEntry(id string, next time.Time, now time.Time) RemoteSubscription {
	entry := RemoteSubscription{RailSubscriptionID: id, Status: SubscriptionStatusUnknown}
	if !next.IsZero() {
		n := next
		entry.NextBillingAt = &n
		entry.Status = SubscriptionStatusActive
		if next.Before(now.Truncate(24 * time.Hour)) {
			entry.Status = SubscriptionStatusPastDue
		}
	}
	return entry
}

// attribution assigns sales to the rows they paid. NMI's transaction report
// usually names no schedule, so a sale is attributed by its vault, then its
// order reference; a sale two rows could claim marks them for a single read.
type attribution struct {
	byRail    map[string]*models.Subscription
	byVault   map[string][]*models.Subscription
	byOrder   map[string][]*models.Subscription
	txns      map[uuid.UUID][]RemoteTransaction
	ambiguous map[uuid.UUID]bool
	only      *models.Subscription
}

func newAttribution(subs []*models.Subscription, vaults map[uuid.UUID]string, records map[string]nmi.ScheduleRecord) *attribution {
	a := &attribution{byRail: map[string]*models.Subscription{}, byVault: map[string][]*models.Subscription{}, byOrder: map[string][]*models.Subscription{},
		txns: map[uuid.UUID][]RemoteTransaction{}, ambiguous: map[uuid.UUID]bool{}}
	for _, s := range subs {
		a.byRail[s.RailSubscriptionID] = s
		if v := vaults[s.ID]; v != "" {
			a.byVault[v] = append(a.byVault[v], s)
		} else if len(subs) > 1 {
			a.ambiguous[s.ID] = true // no known vault: read it by its own schedule id
		}
		a.byOrder[s.ID.String()] = append(a.byOrder[s.ID.String()], s)
		if o := records[s.RailSubscriptionID].OrderID; o != "" && o != s.ID.String() {
			a.byOrder[o] = append(a.byOrder[o], s)
		}
	}
	if len(subs) == 1 {
		a.only = subs[0]
	}
	return a
}

func (a *attribution) add(sale nmi.ScheduleSale) {
	var target *models.Subscription
	switch vault := a.byVault[sale.VaultID]; {
	case a.byRail[sale.SubscriptionID] != nil:
		target = a.byRail[sale.SubscriptionID]
	case a.only != nil:
		target = a.only
	case len(vault) == 1:
		target = vault[0]
	case len(a.byOrder[sale.OrderID]) == 1 && sale.OrderID != "":
		target = a.byOrder[sale.OrderID][0]
	default:
		for _, s := range vault {
			a.ambiguous[s.ID] = true
		}
		return
	}
	a.txns[target.ID] = append(a.txns[target.ID], saleTransaction(sale, target.RailSubscriptionID))
}

func saleTransaction(sale nmi.ScheduleSale, railSubID string) RemoteTransaction {
	amount, _ := parseAmountCents(sale.Amount)
	t := RemoteTransaction{TransactionID: sale.TransactionID, SubscriptionID: railSubID, Type: TransactionTypeSale, Success: sale.Success,
		AmountCents: amount, Currency: sale.Currency, OccurredAt: sale.At}
	if !sale.Success {
		t.Type, t.DeclineReason, t.DeclineCode = TransactionTypeDecline, sale.ResponseText, sale.ResponseCode
	}
	return t
}

// vaultsOf maps each row to the NMI vault its stored card lives in.
func vaultsOf(ctx context.Context, database *db.DB, subs []*models.Subscription) (map[uuid.UUID]string, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(subs))
	for _, s := range subs {
		ids = append(ids, s.ID)
	}
	rows, err := database.Qx(ctx).Query(ctx, `SELECT s.id, pm.rail_customer_ref FROM openrails.subscriptions s
		JOIN openrails.payment_methods pm ON pm.merchant_id = s.merchant_id AND pm.id = s.payment_method_id
		WHERE s.merchant_id = $1 AND s.id = ANY($2) AND s.deleted_at IS NULL`, mid.UUID(), ids)
	if err != nil {
		return nil, fmt.Errorf("verify: load vaults: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var vault string
		if err := rows.Scan(&id, &vault); err != nil {
			return nil, err
		}
		out[id] = strings.TrimSpace(vault)
	}
	return out, rows.Err()
}

// Bulk resolves every unverified row of the merchant's NMI account from one
// paged roster read and one paged transaction read by date range (§12 bulk
// mode), instead of per-row reads. Each transaction page's charges are
// recorded as payments and checkpointed, so a crashed pass resumes at the
// next page; rows are then decided from their recorded charges. Rows the
// bulk read cannot attribute fall back to batched reads.
func (v *Verifier) Bulk(ctx context.Context, mid merchant.ID) error {
	v.init()
	v.mu.Lock()
	if _, running := v.bulking[mid]; running {
		v.mu.Unlock()
		return nil // the running bulk read covers the account
	}
	v.bulking[mid] = map[uuid.UUID]struct{}{}
	v.mu.Unlock()
	defer v.endBulk(mid)
	return v.bulkRead(ctx, mid)
}

func (v *Verifier) bulkRead(ctx context.Context, mid merchant.ID) error {
	return v.DB.RunInMerchantConn(merchant.WithID(ctx, mid), func(ctx context.Context) error {
		now := v.Clock.Now().UTC()
		armed := v.Builder.Build(ctx, mid)
		client, psp := nmiReader(armed)
		if client == nil {
			return nil
		}
		subs, err := listUnverifiedNMI(ctx, v.DB, mid, psp)
		if err != nil {
			return err
		}
		v.cover(mid, subs)
		if len(subs) == 0 {
			return clearCheckpoint(ctx, v.DB, mid, psp)
		}
		roster, err := readRoster(ctx, client)
		if err != nil {
			return fmt.Errorf("nmi roster: %w", err)
		}
		live := map[string][]string{} // vault -> live schedules
		for _, s := range roster {
			live[strings.TrimSpace(s.CustomerVaultID)] = append(live[strings.TrimSpace(s.CustomerVaultID)], s.ID)
		}
		byRail := map[string]*models.Subscription{}
		since := now.Add(-3 * 365 * 24 * time.Hour)
		earliest := now
		for _, s := range subs {
			byRail[s.RailSubscriptionID] = s
			if s.CurrentPeriodEndsAt != nil {
				earliest = minTime(earliest, s.CurrentPeriodEndsAt.UTC().Add(-AlignmentSlack(s.CurrentPeriodStartsAt, s.CurrentPeriodEndsAt)))
			}
		}
		since = maxTime(since, earliest)
		cp, err := loadCheckpoint(ctx, v.DB, mid, psp, since, now)
		if err != nil {
			return err
		}
		q := v.DB.Gen(ctx)
		for page := cp.next; ; page++ {
			sales, n, err := client.SalesPage(ctx, cp.since, cp.until, page)
			if err != nil {
				return fmt.Errorf("nmi transaction page %d: %w", page, err)
			}
			perSub := map[*models.Subscription][]RemoteTransaction{}
			for _, sale := range sales {
				id := sale.SubscriptionID
				if id == "" {
					if schedules := live[sale.VaultID]; len(schedules) == 1 {
						id = schedules[0]
					}
				}
				if sub := byRail[id]; sub != nil {
					perSub[sub] = append(perSub[sub], saleTransaction(sale, sub.RailSubscriptionID))
				}
			}
			for sub, txns := range perSub {
				if _, err := backfillSubscriptionPayments(ctx, q, sub, txns, now, defaultBackfillLookback); err != nil {
					return err
				}
			}
			if err := saveCheckpoint(ctx, v.DB, mid, psp, page+1); err != nil {
				return err
			}
			if n < nmi.QueryPageLimit {
				break
			}
		}

		charges, err := recordedCharges(ctx, v.DB, mid, subs, cp.since)
		if err != nil {
			return err
		}
		rosterByID := map[string]nmi.V5Subscription{}
		for _, s := range roster {
			rosterByID[s.ID] = s
		}
		cov := armed.Coverage[ProviderNMI]
		localLive, err := q.CountLiveLinkedSubscriptionsForRail(ctx, gen.CountLiveLinkedSubscriptionsForRailParams{MerchantID: mid.UUID(), Rail: string(models.RailNMI)})
		if err != nil {
			return err
		}
		tripped, _ := RosterBreaker{}.Implausible(ProviderNMI, len(roster), int(localLive))
		exhaustive := len(roster) > 0 && cov.Complete() && !tripped
		snaps := map[uuid.UUID]*RemoteSnapshot{}
		var fallback []*models.Subscription
		for _, sub := range subs {
			entry, listed := rosterByID[sub.RailSubscriptionID]
			if listed && len(live[strings.TrimSpace(entry.CustomerVaultID)]) > 1 {
				fallback = append(fallback, sub) // its charges could belong to a sibling schedule
				continue
			}
			if !listed && !exhaustive {
				fallback = append(fallback, sub)
				continue
			}
			snap := &RemoteSnapshot{Provider: ProviderNMI, FetchedAt: now, Transactions: charges[sub.ID],
				Coverage:     SnapshotCoverage{SubscriptionsExhaustive: exhaustive},
				Capabilities: Capabilities{Subscriptions: true, Transactions: true}}
			if listed {
				next, _ := parseNMIV5Date(entry.NextBillingDate)
				snap.Subscriptions = []RemoteSubscription{nmiRosterEntry(sub.RailSubscriptionID, next, now)}
			}
			snaps[sub.ID] = snap
		}
		lc := v.lifecycle()
		errs := applyVerified(ctx, v.DB, lc, subs, snaps, now)
		errs = errors.Join(errs, resolveNMIBatch(ctx, v.DB, lc, client, fallback, now))
		recordReads(ctx, v.DB, mid, subs, now, errs)
		if errs != nil {
			return errs
		}
		return clearCheckpoint(ctx, v.DB, mid, psp)
	})
}

func readRoster(ctx context.Context, client *nmi.NMIClient) ([]nmi.V5Subscription, error) {
	var out []nmi.V5Subscription
	cursor, seen := "", map[string]bool{}
	for {
		page, err := client.ListSubscriptionsPage(ctx, cursor, nmiV5PageLimit)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Subscriptions...)
		next := string(page.NextCursor)
		if !page.HasMore || next == "" {
			return out, nil
		}
		if seen[next] {
			return nil, fmt.Errorf("nmi subscription pagination repeated cursor %s", next)
		}
		seen[next], cursor = true, next
	}
}

func listUnverifiedNMI(ctx context.Context, database *db.DB, mid merchant.ID, psp uuid.UUID) ([]*models.Subscription, error) {
	rows, err := database.Qx(ctx).Query(ctx, `SELECT id FROM openrails.subscriptions
		WHERE merchant_id = $1 AND psp_id = $2 AND status = 'unverified' AND deleted_at IS NULL
		  AND collection_policy <> 'engine' AND rail_subscription_id <> ''
		ORDER BY current_period_ends_at NULLS FIRST`, mid.UUID(), psp)
	if err != nil {
		return nil, fmt.Errorf("verify: list unverified nmi rows: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	return loadUnverified(ctx, database, mid, ids)
}

// recordedCharges is each row's recorded charges and declines since, the
// evidence a bulk pass decides from.
func recordedCharges(ctx context.Context, database *db.DB, mid merchant.ID, subs []*models.Subscription, since time.Time) (map[uuid.UUID][]RemoteTransaction, error) {
	ids := make([]uuid.UUID, 0, len(subs))
	rail := map[uuid.UUID]string{}
	for _, s := range subs {
		ids = append(ids, s.ID)
		rail[s.ID] = s.RailSubscriptionID
	}
	rows, err := database.Qx(ctx).Query(ctx, `SELECT subscription_id, transaction_id, status, purchased_at, amount, currency, COALESCE(failure_code, '')
		FROM openrails.payments
		WHERE merchant_id = $1 AND subscription_id = ANY($2) AND purchased_at >= $3 AND status IN ('completed', 'failed') AND deleted_at IS NULL`, mid.UUID(), ids, since)
	if err != nil {
		return nil, fmt.Errorf("verify: load recorded charges: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID][]RemoteTransaction{}
	for rows.Next() {
		var (
			sub                         uuid.UUID
			txn, status, currency, code string
			at                          time.Time
			micros                      int64
		)
		if err := rows.Scan(&sub, &txn, &status, &at, &micros, &currency, &code); err != nil {
			return nil, err
		}
		t := RemoteTransaction{TransactionID: txn, SubscriptionID: rail[sub], Type: TransactionTypeSale, Success: status == "completed",
			AmountCents: micros / 10_000, Currency: currency, OccurredAt: at.UTC()}
		if !t.Success {
			t.Type, t.DeclineCode = TransactionTypeDecline, code
		}
		out[sub] = append(out[sub], t)
	}
	return out, rows.Err()
}

type bulkCheckpoint struct {
	since, until time.Time
	next         int
}

// loadCheckpoint resumes an interrupted bulk read, or starts a new one.
func loadCheckpoint(ctx context.Context, database *db.DB, mid merchant.ID, psp uuid.UUID, since, until time.Time) (bulkCheckpoint, error) {
	cp := bulkCheckpoint{since: since, until: until, next: 1}
	err := database.Qx(ctx).QueryRow(ctx, `INSERT INTO openrails.nmi_bulk_checkpoints (merchant_id, psp_id, since, until, next_page, started_at)
		VALUES ($1, $2, $3, $4, 1, $4)
		ON CONFLICT (merchant_id, psp_id) DO UPDATE SET merchant_id = EXCLUDED.merchant_id
		RETURNING since, until, next_page`, mid.UUID(), psp, since, until).Scan(&cp.since, &cp.until, &cp.next)
	if err != nil {
		return cp, fmt.Errorf("verify: bulk checkpoint: %w", err)
	}
	return cp, nil
}

func saveCheckpoint(ctx context.Context, database *db.DB, mid merchant.ID, psp uuid.UUID, next int) error {
	_, err := database.Qx(ctx).Exec(ctx, `UPDATE openrails.nmi_bulk_checkpoints SET next_page = $3 WHERE merchant_id = $1 AND psp_id = $2`, mid.UUID(), psp, next)
	return err
}

func clearCheckpoint(ctx context.Context, database *db.DB, mid merchant.ID, psp uuid.UUID) error {
	_, err := database.Qx(ctx).Exec(ctx, `DELETE FROM openrails.nmi_bulk_checkpoints WHERE merchant_id = $1 AND psp_id = $2`, mid.UUID(), psp)
	return err
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
