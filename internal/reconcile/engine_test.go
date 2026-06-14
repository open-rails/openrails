package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fakes -----------------------------------------------------------------

type fakeFetcher struct {
	provider Provider
	snap     *RemoteSnapshot
	err      error
}

func (f *fakeFetcher) Name() string { return string(f.provider) }
func (f *fakeFetcher) Capabilities() Capabilities {
	if f.snap != nil {
		return f.snap.Capabilities
	}
	return Capabilities{}
}
func (f *fakeFetcher) Fetch(ctx context.Context, params FetchParams) (*RemoteSnapshot, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.snap, nil
}

// fakeLocal serves LocalState and the payment lookup from in-memory slices;
// the fakeWriter mutates the same slices so enforce convergence is visible to
// the next run.
type fakeLocal struct {
	mu       sync.Mutex
	state    LocalState
	payments []LocalPayment
}

func (l *fakeLocal) Load(ctx context.Context, provider Provider) (*LocalState, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := LocalState{
		Subscriptions:  append([]LocalSubscription(nil), l.state.Subscriptions...),
		Entitlements:   append([]LocalEntitlement(nil), l.state.Entitlements...),
		PaymentMethods: append([]LocalPaymentMethod(nil), l.state.PaymentMethods...),
		Prices:         append([]LocalPrice(nil), l.state.Prices...),
	}
	return &cp, nil
}

func (l *fakeLocal) PaymentsByTransactionIDs(ctx context.Context, provider Provider, ids []string) ([]LocalPayment, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []LocalPayment
	for _, p := range l.payments {
		if want[p.TransactionID] {
			out = append(out, p)
		}
	}
	return out, nil
}

// memStore mirrors the SQL upsert/auto-resolve semantics in memory.
type memStore struct {
	mu       sync.Mutex
	runs     map[uuid.UUID]*RunRecord
	findings map[string]*FindingRecord // identity key -> record
	byID     map[uuid.UUID]*FindingRecord
}

func newMemStore() *memStore {
	return &memStore{
		runs:     map[uuid.UUID]*RunRecord{},
		findings: map[string]*FindingRecord{},
		byID:     map[uuid.UUID]*FindingRecord{},
	}
}

func identityKey(provider Provider, t FindingType, subject string) string {
	return string(provider) + "|" + string(t) + "|" + subject
}

func (s *memStore) CreateRun(ctx context.Context, mode Mode, providers []Provider, since, until *time.Time) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := uuid.New()
	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, string(p))
	}
	s.runs[id] = &RunRecord{ID: id, Mode: mode, Providers: names, WindowSince: since, WindowUntil: until, StartedAt: time.Now(), Status: "running"}
	return id, nil
}

func (s *memStore) FinishRun(ctx context.Context, runID uuid.UUID, status string, summary []byte, runErr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	if !ok {
		return fmt.Errorf("no run %s", runID)
	}
	now := time.Now()
	run.Status = status
	run.FinishedAt = &now
	run.Summary = json.RawMessage(summary)
	run.Error = runErr
	return nil
}

func (s *memStore) UpsertFinding(ctx context.Context, runID uuid.UUID, f Finding) (FindingRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := identityKey(f.Provider, f.Type, f.SubjectKey)
	now := time.Now()
	rec, ok := s.findings[key]
	if !ok {
		rec = &FindingRecord{
			ID: uuid.New(), Provider: f.Provider, Type: f.Type, SubjectKey: f.SubjectKey,
			FirstSeenRun: runID, FirstSeenAt: now, OccurrenceCount: 0,
		}
		s.findings[key] = rec
		s.byID[rec.ID] = rec
	}
	rec.Severity = f.Severity
	rec.RequiresAdmin = f.RequiresAdmin
	rec.RecommendedAction = f.RecommendedAction
	rec.LocalEvidence = f.LocalEvidence
	rec.RemoteEvidence = f.RemoteEvidence
	rec.IntentEvidence = f.IntentEvidence
	if rec.Status != FindingStatusDismissed {
		rec.Status = f.Status
		rec.ResolvedAt = nil
		rec.Resolution = ""
		rec.ResolutionEvid = nil
	}
	rec.LastSeenRun = runID
	rec.LastSeenAt = now
	rec.OccurrenceCount++
	return *rec, nil
}

func (s *memStore) ListOpenFindingsByProvider(ctx context.Context, provider Provider) ([]FindingRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []FindingRecord
	for _, rec := range s.findings {
		if rec.Provider == provider && (rec.Status == FindingStatusOpen || rec.Status == FindingStatusAdminPending) {
			out = append(out, *rec)
		}
	}
	return out, nil
}

func (s *memStore) AutoResolveVanished(ctx context.Context, provider Provider, runID uuid.UUID, types []FindingType) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	typeSet := map[FindingType]bool{}
	for _, t := range types {
		typeSet[t] = true
	}
	var n int64
	now := time.Now()
	for _, rec := range s.findings {
		if rec.Provider != provider || !typeSet[rec.Type] {
			continue
		}
		if rec.Status != FindingStatusOpen && rec.Status != FindingStatusAdminPending {
			continue
		}
		if rec.LastSeenRun == runID {
			continue
		}
		rec.Status = FindingStatusResolved
		rec.Resolution = "auto_vanished"
		rec.ResolvedAt = &now
		n++
	}
	return n, nil
}

func (s *memStore) AutoResolveVanishedAllProviders(ctx context.Context, runID uuid.UUID, types []FindingType) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	typeSet := map[FindingType]bool{}
	for _, t := range types {
		typeSet[t] = true
	}
	var n int64
	now := time.Now()
	for _, rec := range s.findings {
		if !typeSet[rec.Type] {
			continue
		}
		if rec.Status != FindingStatusOpen && rec.Status != FindingStatusAdminPending {
			continue
		}
		if rec.LastSeenRun == runID {
			continue
		}
		rec.Status = FindingStatusResolved
		rec.Resolution = "auto_vanished"
		rec.ResolvedAt = &now
		n++
	}
	return n, nil
}

func (s *memStore) MarkFindingVanished(ctx context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[id]
	if !ok {
		return fmt.Errorf("no finding %s", id)
	}
	now := time.Now()
	rec.Status = FindingStatusResolved
	rec.Resolution = "auto_vanished"
	rec.ResolvedAt = &now
	return nil
}

func (s *memStore) MarkFindingAutoFixed(ctx context.Context, id uuid.UUID, evidence map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[id]
	if !ok {
		return fmt.Errorf("no finding %s", id)
	}
	now := time.Now()
	rec.Status = FindingStatusAutoFixed
	rec.Resolution = "enforced"
	rec.ResolutionEvid = evidence
	rec.ResolvedAt = &now
	return nil
}

func (s *memStore) record(provider Provider, t FindingType, subject string) *FindingRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findings[identityKey(provider, t, subject)]
}

func (s *memStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.findings)
}

// fakeWriter applies enforce actions onto the fakeLocal state, counting calls.
type fakeWriter struct {
	local *fakeLocal
	calls map[string]int
}

func newFakeWriter(local *fakeLocal) *fakeWriter {
	return &fakeWriter{local: local, calls: map[string]int{}}
}

func (w *fakeWriter) totalCalls() int {
	n := 0
	for _, c := range w.calls {
		n += c
	}
	return n
}

func (w *fakeWriter) CancelSubscriptionLocal(ctx context.Context, a CancelLocalAction) (bool, error) {
	w.calls["cancel"]++
	w.local.mu.Lock()
	defer w.local.mu.Unlock()
	for i := range w.local.state.Subscriptions {
		s := &w.local.state.Subscriptions[i]
		if s.ID == a.SubscriptionID && s.IsLive() {
			now := time.Now()
			s.Status = "cancelled"
			s.CancelType = a.CancelType
			s.CancelledAt = &now
			return true, nil
		}
	}
	return false, nil
}

func (w *fakeWriter) AdoptSubscriptionStatus(ctx context.Context, a AdoptStatusAction) (bool, error) {
	w.calls["adopt_status"]++
	w.local.mu.Lock()
	defer w.local.mu.Unlock()
	for i := range w.local.state.Subscriptions {
		s := &w.local.state.Subscriptions[i]
		if s.ID != a.SubscriptionID || s.Status == "cancelled" {
			continue
		}
		changed := false
		if s.Status != a.Status {
			s.Status = a.Status
			changed = true
		}
		if a.PeriodEndsAt != nil && (s.CurrentPeriodEndsAt == nil || !s.CurrentPeriodEndsAt.Equal(*a.PeriodEndsAt)) {
			s.CurrentPeriodEndsAt = a.PeriodEndsAt
			changed = true
		}
		if a.PeriodStartsAt != nil && (s.CurrentPeriodStartsAt == nil || !s.CurrentPeriodStartsAt.Equal(*a.PeriodStartsAt)) {
			s.CurrentPeriodStartsAt = a.PeriodStartsAt
			changed = true
		}
		return changed, nil
	}
	return false, nil
}

func (w *fakeWriter) BackfillPayment(ctx context.Context, a BackfillPaymentAction) (bool, error) {
	w.calls["backfill_payment"]++
	w.local.mu.Lock()
	for _, p := range w.local.payments {
		if p.TransactionID == a.TransactionID {
			w.local.mu.Unlock()
			return false, nil
		}
	}
	w.local.payments = append(w.local.payments, LocalPayment{
		ID: uuid.New(), MerchantSubjectID: a.MerchantSubjectID, Processor: a.Processor,
		TransactionID: a.TransactionID, AmountCents: a.AmountCents, Status: "completed",
		SubscriptionID: a.SubscriptionID, PurchasedAt: a.PurchasedAt,
	})
	w.local.mu.Unlock()
	if a.Grant != nil {
		if _, err := w.GrantEntitlements(ctx, *a.Grant); err != nil {
			return true, err
		}
	}
	return true, nil
}

func (w *fakeWriter) RecordRefund(ctx context.Context, a RecordRefundAction) (bool, error) {
	w.calls["record_refund"]++
	w.local.mu.Lock()
	defer w.local.mu.Unlock()
	if a.MarkRefundedOnly {
		for i := range w.local.payments {
			if a.RefundedPaymentID != nil && w.local.payments[i].ID == *a.RefundedPaymentID && w.local.payments[i].Status != "refunded" {
				w.local.payments[i].Status = "refunded"
				return true, nil
			}
		}
		return false, nil
	}
	for _, p := range w.local.payments {
		if p.TransactionID == a.TransactionID {
			return false, nil
		}
	}
	w.local.payments = append(w.local.payments, LocalPayment{
		ID: uuid.New(), MerchantSubjectID: a.MerchantSubjectID, Processor: a.Processor,
		TransactionID: a.TransactionID, AmountCents: -a.AmountCents, Status: "completed",
		SubscriptionID: a.SubscriptionID, RefundedPaymentID: a.RefundedPaymentID, PurchasedAt: a.PurchasedAt,
	})
	if a.RefundedPaymentID != nil {
		for i := range w.local.payments {
			if w.local.payments[i].ID == *a.RefundedPaymentID {
				w.local.payments[i].Status = "refunded"
			}
		}
	}
	return true, nil
}

func (w *fakeWriter) AdoptPaymentMethod(ctx context.Context, a AdoptVaultAction) (bool, error) {
	w.calls["adopt_vault"]++
	w.local.mu.Lock()
	defer w.local.mu.Unlock()
	for i := range w.local.state.PaymentMethods {
		pm := &w.local.state.PaymentMethods[i]
		if pm.ID != a.PaymentMethodID {
			continue
		}
		changed := false
		if a.LastFour != "" && pm.LastFour != a.LastFour {
			pm.LastFour = a.LastFour
			changed = true
		}
		if a.ExpiryDate != "" && normalizeExpiry(pm.ExpiryDate) != normalizeExpiry(a.ExpiryDate) {
			pm.ExpiryDate = a.ExpiryDate
			changed = true
		}
		return changed, nil
	}
	return false, nil
}

func (w *fakeWriter) GrantEntitlements(ctx context.Context, a GrantEntitlementsAction) (int, error) {
	w.calls["grant"]++
	w.local.mu.Lock()
	defer w.local.mu.Unlock()
	granted := 0
	for _, name := range a.Entitlements {
		exists := false
		for _, ent := range w.local.state.Entitlements {
			if ent.SourceID == a.SubscriptionID && ent.Entitlement == name {
				exists = true
				break
			}
		}
		if exists {
			continue
		}
		w.local.state.Entitlements = append(w.local.state.Entitlements, LocalEntitlement{
			ID: uuid.New(), MerchantSubjectID: a.MerchantSubjectID, Entitlement: name,
			SourceID: a.SubscriptionID, StartAt: a.StartAt, EndAt: a.EndAt,
		})
		granted++
	}
	return granted, nil
}

// fakeMaterializeEntitlements is what the fakeWriter "snapshots" onto every
// materialized subscription (the PG writer reads the product's spec).
var fakeMaterializeEntitlements = []string{"premium"}

func (w *fakeWriter) MaterializeSubscription(ctx context.Context, a MaterializeSubscriptionAction) (MaterializeResult, error) {
	w.calls["materialize"]++
	w.local.mu.Lock()
	for i := range w.local.state.Subscriptions {
		if w.local.state.Subscriptions[i].ProcessorSubscriptionID == a.ProcessorSubscriptionID {
			w.local.mu.Unlock()
			return MaterializeResult{}, nil // already materialized
		}
	}
	priceID := a.PriceID
	sub := LocalSubscription{
		ID:                      uuid.New(),
		MerchantSubjectID:         a.MerchantSubjectID,
		PriceID:                 &priceID,
		ProductID:               a.ProductID,
		Status:                  a.Status,
		Processor:               a.Processor,
		ProcessorSubscriptionID: a.ProcessorSubscriptionID,
		UserEmail:               a.UserEmail,
		CurrentPeriodStartsAt:   a.PeriodStartsAt,
		CurrentPeriodEndsAt:     a.PeriodEndsAt,
		StartedAt:               time.Now(),
		EntitlementNames:        fakeMaterializeEntitlements,
	}
	w.local.state.Subscriptions = append(w.local.state.Subscriptions, sub)
	w.local.mu.Unlock()

	res := MaterializeResult{SubscriptionID: sub.ID, Created: true}
	if a.PeriodEndsAt == nil || a.PeriodEndsAt.After(time.Now()) {
		start := time.Now()
		if a.PeriodStartsAt != nil {
			start = *a.PeriodStartsAt
		}
		granted, err := w.GrantEntitlements(ctx, GrantEntitlementsAction{
			SubscriptionID:  sub.ID,
			MerchantSubjectID: a.MerchantSubjectID,
			Entitlements:    fakeMaterializeEntitlements,
			StartAt:         start,
			EndAt:           a.PeriodEndsAt,
		})
		if err != nil {
			return res, err
		}
		res.EntitlementsGranted = granted
	}
	if a.Backfill != nil {
		b := *a.Backfill
		subID := sub.ID
		b.SubscriptionID = &subID
		backfilled, err := w.BackfillPayment(ctx, b)
		if err != nil {
			return res, err
		}
		res.PaymentBackfilled = backfilled
	}
	return res, nil
}

func (w *fakeWriter) RevokeSubscriptionEntitlements(ctx context.Context, a RevokeEntitlementsAction) (int, error) {
	w.calls["revoke"]++
	w.local.mu.Lock()
	defer w.local.mu.Unlock()
	var kept []LocalEntitlement
	revoked := 0
	for _, ent := range w.local.state.Entitlements {
		if ent.SourceID == a.SubscriptionID {
			revoked++
			continue
		}
		kept = append(kept, ent)
	}
	w.local.state.Entitlements = kept
	return revoked, nil
}

// --- helpers ----------------------------------------------------------------

func tp(t time.Time) *time.Time { return &t }

var testNow = time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)

func newTestEngine(provider Provider, snap *RemoteSnapshot, local *fakeLocal) (*Engine, *memStore, *fakeWriter) {
	store := newMemStore()
	writer := newFakeWriter(local)
	eng := &Engine{
		Fetchers: map[Provider]ProcessorFetcher{provider: &fakeFetcher{provider: provider, snap: snap}},
		Store:    store,
		Local:    local,
		Writer:   writer,
		Now:      func() time.Time { return testNow },
	}
	return eng, store, writer
}

func liveLocalSub(provider Provider, psid string) LocalSubscription {
	priceID := uuid.New()
	return LocalSubscription{
		ID:                      uuid.New(),
		MerchantSubjectID:         uuid.New(),
		PriceID:                 &priceID,
		ProductID:               uuid.New(),
		Status:                  "active",
		Processor:               string(provider),
		ProcessorSubscriptionID: psid,
		CurrentPeriodStartsAt:   tp(testNow.Add(-10 * 24 * time.Hour)),
		CurrentPeriodEndsAt:     tp(testNow.Add(20 * 24 * time.Hour)),
		StartedAt:               testNow.Add(-100 * 24 * time.Hour),
		EntitlementNames:        []string{"premium"},
	}
}

func withLiveEntitlement(local *fakeLocal, s *LocalSubscription) {
	local.state.Entitlements = append(local.state.Entitlements, LocalEntitlement{
		ID: uuid.New(), MerchantSubjectID: s.MerchantSubjectID, Entitlement: "premium",
		SourceID: s.ID, StartAt: testNow.Add(-10 * 24 * time.Hour), EndAt: s.CurrentPeriodEndsAt,
	})
}

func nmiSaleTxn(txnID, orderID string, success bool) RemoteTransaction {
	t := RemoteTransaction{
		TransactionID: txnID,
		Type:          TransactionTypeSale,
		Success:       success,
		AmountCents:   999,
		Currency:      "USD",
		OccurredAt:    testNow.Add(-48 * time.Hour),
		Raw:           rawJSON(map[string]any{"order_id": orderID}),
	}
	if !success {
		t.Type = TransactionTypeDecline
		t.DeclineReason = "Insufficient funds"
	}
	return t
}

func findByType(findings []FindingRecord, t FindingType) []FindingRecord {
	var out []FindingRecord
	for _, f := range findings {
		if f.Type == t {
			out = append(out, f)
		}
	}
	return out
}

// --- taxonomy ----------------------------------------------------------------

func TestDiffTaxonomy(t *testing.T) {
	ctx := context.Background()

	t.Run("PS-1 processor sub missing locally is critical admin_pending with no apply", func(t *testing.T) {
		local := &fakeLocal{}
		known := liveLocalSub(ProviderNMI, "known-1")
		local.state.Subscriptions = []LocalSubscription{known}
		withLiveEntitlement(local, &known)
		snap := &RemoteSnapshot{
			Provider:     ProviderNMI,
			Capabilities: Capabilities{Subscriptions: true},
			Subscriptions: []RemoteSubscription{
				{ProcessorSubscriptionID: "known-1", Status: SubscriptionStatusActive, NextBillingAt: tp(*known.CurrentPeriodEndsAt)},
				{ProcessorSubscriptionID: "ghost-7", Status: SubscriptionStatusActive, Email: "ghost@example.com"},
			},
		}
		eng, store, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)

		ps1 := findByType(res.Findings, FindingRemoteSubMissingLocal)
		require.Len(t, ps1, 1)
		assert.Equal(t, "ghost-7", ps1[0].SubjectKey)
		assert.Equal(t, SeverityCritical, ps1[0].Severity)
		assert.Equal(t, FindingStatusAdminPending, ps1[0].Status)
		assert.True(t, ps1[0].RequiresAdmin)
		// Never auto-created, even in enforce mode.
		assert.Equal(t, FindingStatusAdminPending, store.record(ProviderNMI, FindingRemoteSubMissingLocal, "ghost-7").Status)
		assert.Zero(t, writer.totalCalls())
	})

	t.Run("PS-1 email fallback surfaces the candidate but never auto-links", func(t *testing.T) {
		local := &fakeLocal{}
		known := liveLocalSub(ProviderNMI, "known-1")
		known.UserEmail = "jane@example.com"
		local.state.Subscriptions = []LocalSubscription{known}
		snap := &RemoteSnapshot{
			Provider:     ProviderNMI,
			Capabilities: Capabilities{Subscriptions: true},
			Subscriptions: []RemoteSubscription{
				{ProcessorSubscriptionID: "known-1", Status: SubscriptionStatusActive},
				{ProcessorSubscriptionID: "ghost-9", Status: SubscriptionStatusActive, Email: "JANE@example.com"},
			},
		}
		eng, _, _ := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)
		ps1 := findByType(res.Findings, FindingRemoteSubMissingLocal)
		require.Len(t, ps1, 1)
		require.NotNil(t, ps1[0].LocalEvidence)
		candidates, ok := ps1[0].LocalEvidence["email_candidates"].([]map[string]any)
		require.True(t, ok)
		require.Len(t, candidates, 1)
		assert.Equal(t, known.ID.String(), candidates[0]["subscription_id"])
		assert.Equal(t, FindingStatusAdminPending, ps1[0].Status)
	})

	t.Run("PS-2 NMI absence from recurring report cancels locally on enforce", func(t *testing.T) {
		local := &fakeLocal{}
		dead := liveLocalSub(ProviderNMI, "vanished-1")
		alive := liveLocalSub(ProviderNMI, "alive-1")
		local.state.Subscriptions = []LocalSubscription{dead, alive}
		withLiveEntitlement(local, &dead)
		withLiveEntitlement(local, &alive)
		snap := &RemoteSnapshot{
			Provider:     ProviderNMI,
			Capabilities: Capabilities{Subscriptions: true},
			Subscriptions: []RemoteSubscription{
				{ProcessorSubscriptionID: "alive-1", Status: SubscriptionStatusActive, NextBillingAt: tp(*alive.CurrentPeriodEndsAt)},
			},
		}
		eng, store, writer := newTestEngine(ProviderNMI, snap, local)
		// Below the breaker threshold (2 local live), so absence is honored.
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)

		ps2 := findByType(res.Findings, FindingLocalActiveRemoteDead)
		require.Len(t, ps2, 1)
		assert.Equal(t, dead.ID.String(), ps2[0].SubjectKey)
		assert.Equal(t, SeverityHigh, ps2[0].Severity)

		rec := store.record(ProviderNMI, FindingLocalActiveRemoteDead, dead.ID.String())
		require.NotNil(t, rec)
		assert.Equal(t, FindingStatusAutoFixed, rec.Status)
		assert.Equal(t, "enforced", rec.Resolution)
		assert.Equal(t, 1, writer.calls["cancel"])
		assert.Equal(t, 1, writer.calls["revoke"])

		// Local state converged: the subscription is cancelled, entitlements gone.
		st, _ := local.Load(ctx, ProviderNMI)
		for _, s := range st.Subscriptions {
			if s.ID == dead.ID {
				assert.Equal(t, "cancelled", s.Status)
				assert.Equal(t, "expired", s.CancelType)
			}
		}
		for _, ent := range st.Entitlements {
			assert.NotEqual(t, dead.ID, ent.SourceID, "the dead subscription's entitlements must be revoked")
		}
	})

	t.Run("PS-2 presence-based on stripe", func(t *testing.T) {
		local := &fakeLocal{}
		sub := liveLocalSub(ProviderStripe, "sub_123")
		local.state.Subscriptions = []LocalSubscription{sub}
		snap := &RemoteSnapshot{
			Provider:     ProviderStripe,
			Capabilities: Capabilities{Subscriptions: true},
			Subscriptions: []RemoteSubscription{
				{ProcessorSubscriptionID: "sub_123", Status: SubscriptionStatusCancelled, RawStatus: "canceled"},
			},
		}
		eng, _, _ := newTestEngine(ProviderStripe, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderStripe}})
		require.NoError(t, err)
		ps2 := findByType(res.Findings, FindingLocalActiveRemoteDead)
		require.Len(t, ps2, 1)
		assert.Equal(t, FindingStatusOpen, ps2[0].Status)
	})

	t.Run("PS-2 absence is NOT inferred for ccbill (windowed exports)", func(t *testing.T) {
		local := &fakeLocal{}
		sub := liveLocalSub(ProviderCCBill, "920000001")
		local.state.Subscriptions = []LocalSubscription{sub}
		snap := &RemoteSnapshot{
			Provider:     ProviderCCBill,
			Capabilities: Capabilities{Subscriptions: true, Transactions: true, Refunds: true, Chargebacks: true},
			// Empty ACTIVEMEMBERS + no termination events: proves nothing.
		}
		eng, _, _ := newTestEngine(ProviderCCBill, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderCCBill}})
		require.NoError(t, err)
		assert.Empty(t, findByType(res.Findings, FindingLocalActiveRemoteDead))
	})

	t.Run("PS-3 adopts processor status and periods on enforce", func(t *testing.T) {
		local := &fakeLocal{}
		sub := liveLocalSub(ProviderStripe, "sub_42")
		sub.Status = "past_due"
		local.state.Subscriptions = []LocalSubscription{sub}
		remoteEnd := testNow.Add(25 * 24 * time.Hour)
		snap := &RemoteSnapshot{
			Provider:     ProviderStripe,
			Capabilities: Capabilities{Subscriptions: true},
			Subscriptions: []RemoteSubscription{
				{ProcessorSubscriptionID: "sub_42", Status: SubscriptionStatusActive, RawStatus: "active",
					LastBilledAt: tp(testNow.Add(-5 * 24 * time.Hour)), NextBillingAt: &remoteEnd},
			},
		}
		eng, store, writer := newTestEngine(ProviderStripe, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderStripe}})
		require.NoError(t, err)
		ps3 := findByType(res.Findings, FindingStatusMismatch)
		require.Len(t, ps3, 1)
		assert.Equal(t, 1, writer.calls["adopt_status"])
		rec := store.record(ProviderStripe, FindingStatusMismatch, sub.ID.String())
		assert.Equal(t, FindingStatusAutoFixed, rec.Status)

		st, _ := local.Load(ctx, ProviderStripe)
		assert.Equal(t, "active", st.Subscriptions[0].Status)
		require.NotNil(t, st.Subscriptions[0].CurrentPeriodEndsAt)
		assert.True(t, st.Subscriptions[0].CurrentPeriodEndsAt.Equal(remoteEnd))
	})

	t.Run("PS-3 local-dead remote-live goes to the admin queue (no resurrection)", func(t *testing.T) {
		local := &fakeLocal{}
		sub := liveLocalSub(ProviderStripe, "sub_dead")
		sub.Status = "cancelled"
		sub.CancelType = "user"
		sub.CancelledAt = tp(testNow.Add(-3 * 24 * time.Hour))
		local.state.Subscriptions = []LocalSubscription{sub}
		snap := &RemoteSnapshot{
			Provider:     ProviderStripe,
			Capabilities: Capabilities{Subscriptions: true},
			Subscriptions: []RemoteSubscription{
				{ProcessorSubscriptionID: "sub_dead", Status: SubscriptionStatusActive},
			},
		}
		eng, _, writer := newTestEngine(ProviderStripe, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderStripe}})
		require.NoError(t, err)
		ps3 := findByType(res.Findings, FindingStatusMismatch)
		require.Len(t, ps3, 1)
		assert.Equal(t, FindingStatusAdminPending, ps3[0].Status)
		assert.True(t, ps3[0].RequiresAdmin)
		assert.Empty(t, ps3[0].IntentEvidence)
		assert.Zero(t, writer.totalCalls())
	})

	t.Run("PS-4 backfills correlated charge and grants current-period entitlements", func(t *testing.T) {
		local := &fakeLocal{}
		sub := liveLocalSub(ProviderNMI, "nmi-sub-1")
		local.state.Subscriptions = []LocalSubscription{sub}
		// No live entitlement: the backfill grant should add it.
		snap := &RemoteSnapshot{
			Provider:     ProviderNMI,
			Capabilities: Capabilities{Subscriptions: true, Transactions: true, Refunds: true},
			Subscriptions: []RemoteSubscription{
				{ProcessorSubscriptionID: "nmi-sub-1", Status: SubscriptionStatusActive, NextBillingAt: tp(*sub.CurrentPeriodEndsAt)},
			},
			Transactions: []RemoteTransaction{
				nmiSaleTxn("txn-1001", fmt.Sprintf("rebill-%s-%d", sub.ID, testNow.Unix()), true),
			},
		}
		eng, store, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)

		ps4 := findByType(res.Findings, FindingChargeMissingLocal)
		require.Len(t, ps4, 1)
		assert.Equal(t, "txn-1001", ps4[0].SubjectKey)
		assert.Equal(t, SeverityHigh, ps4[0].Severity)
		assert.Equal(t, 1, writer.calls["backfill_payment"])
		rec := store.record(ProviderNMI, FindingChargeMissingLocal, "txn-1001")
		assert.Equal(t, FindingStatusAutoFixed, rec.Status)

		// Payment exists + entitlement granted for the current period.
		payments, _ := local.PaymentsByTransactionIDs(ctx, ProviderNMI, []string{"txn-1001"})
		require.Len(t, payments, 1)
		assert.Equal(t, sub.MerchantSubjectID, payments[0].MerchantSubjectID)
		st, _ := local.Load(ctx, ProviderNMI)
		require.Len(t, st.Entitlements, 1)
		assert.Equal(t, "premium", st.Entitlements[0].Entitlement)
	})

	t.Run("PS-4 ambiguous identity goes to the admin queue, never guesses", func(t *testing.T) {
		local := &fakeLocal{}
		a := liveLocalSub(ProviderNMI, "nmi-a")
		b := liveLocalSub(ProviderNMI, "nmi-b")
		a.UserEmail = "dup@example.com"
		b.UserEmail = "dup@example.com"
		local.state.Subscriptions = []LocalSubscription{a, b}
		txn := RemoteTransaction{
			TransactionID: "txn-amb", Type: TransactionTypeSale, Success: true, AmountCents: 999,
			OccurredAt: testNow.Add(-time.Hour),
			Raw:        rawJSON(map[string]any{"email": "dup@example.com"}),
		}
		snap := &RemoteSnapshot{
			Provider:     ProviderNMI,
			Capabilities: Capabilities{Subscriptions: true, Transactions: true},
			Subscriptions: []RemoteSubscription{
				{ProcessorSubscriptionID: "nmi-a", Status: SubscriptionStatusActive},
				{ProcessorSubscriptionID: "nmi-b", Status: SubscriptionStatusActive},
			},
			Transactions: []RemoteTransaction{txn},
		}
		eng, _, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)
		ps4 := findByType(res.Findings, FindingChargeMissingLocal)
		require.Len(t, ps4, 1)
		assert.Equal(t, FindingStatusAdminPending, ps4[0].Status)
		assert.True(t, ps4[0].RequiresAdmin)
		assert.Contains(t, ps4[0].RecommendedAction, "MULTIPLE")
		assert.Zero(t, writer.calls["backfill_payment"])
	})

	t.Run("PS-5 NMI refund on the original transaction marks it refunded", func(t *testing.T) {
		local := &fakeLocal{}
		sub := liveLocalSub(ProviderNMI, "nmi-sub-5")
		local.state.Subscriptions = []LocalSubscription{sub}
		subID := sub.ID
		original := LocalPayment{
			ID: uuid.New(), MerchantSubjectID: sub.MerchantSubjectID, Processor: "nmi",
			TransactionID: "txn-5001", AmountCents: 999, Status: "completed",
			SubscriptionID: &subID, PurchasedAt: testNow.Add(-10 * 24 * time.Hour),
		}
		local.payments = []LocalPayment{original}
		refund := RemoteTransaction{
			TransactionID: "txn-5001", Type: TransactionTypeRefund, Success: true,
			AmountCents: 999, OccurredAt: testNow.Add(-time.Hour),
			Raw: rawJSON(map[string]any{"order_id": sub.ID.String()}),
		}
		snap := &RemoteSnapshot{
			Provider:     ProviderNMI,
			Capabilities: Capabilities{Subscriptions: true, Transactions: true, Refunds: true},
			Subscriptions: []RemoteSubscription{
				{ProcessorSubscriptionID: "nmi-sub-5", Status: SubscriptionStatusActive, NextBillingAt: tp(*sub.CurrentPeriodEndsAt)},
			},
			Transactions: []RemoteTransaction{refund},
		}
		eng, store, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)

		ps5 := findByType(res.Findings, FindingRefundUnrecorded)
		require.Len(t, ps5, 1)
		assert.Equal(t, 1, writer.calls["record_refund"])
		rec := store.record(ProviderNMI, FindingRefundUnrecorded, "txn-5001")
		assert.Equal(t, FindingStatusAutoFixed, rec.Status)
		payments, _ := local.PaymentsByTransactionIDs(ctx, ProviderNMI, []string{"txn-5001"})
		require.Len(t, payments, 1)
		assert.Equal(t, "refunded", payments[0].Status)
	})

	t.Run("PS-5 already-recorded refund emits nothing", func(t *testing.T) {
		local := &fakeLocal{}
		sub := liveLocalSub(ProviderStripe, "sub_55")
		local.state.Subscriptions = []LocalSubscription{sub}
		subID := sub.ID
		originalID := uuid.New()
		local.payments = []LocalPayment{
			{ID: originalID, MerchantSubjectID: sub.MerchantSubjectID, Processor: "stripe", TransactionID: "ch_1", AmountCents: 999, Status: "refunded", SubscriptionID: &subID, PurchasedAt: testNow.Add(-9 * 24 * time.Hour)},
			{ID: uuid.New(), MerchantSubjectID: sub.MerchantSubjectID, Processor: "stripe", TransactionID: "re_1", AmountCents: -999, Status: "completed", SubscriptionID: &subID, RefundedPaymentID: &originalID, PurchasedAt: testNow.Add(-8 * 24 * time.Hour)},
		}
		snap := &RemoteSnapshot{
			Provider:     ProviderStripe,
			Capabilities: Capabilities{Subscriptions: true, Transactions: true, Refunds: true, Chargebacks: true},
			Subscriptions: []RemoteSubscription{
				{ProcessorSubscriptionID: "sub_55", Status: SubscriptionStatusActive, NextBillingAt: tp(*sub.CurrentPeriodEndsAt)},
			},
			Transactions: []RemoteTransaction{
				{TransactionID: "re_1", Type: TransactionTypeRefund, Success: true, AmountCents: 999, OccurredAt: testNow.Add(-8 * 24 * time.Hour), Raw: rawJSON(map[string]any{"charge": "ch_1"})},
			},
		}
		eng, _, _ := newTestEngine(ProviderStripe, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderStripe}})
		require.NoError(t, err)
		assert.Empty(t, findByType(res.Findings, FindingRefundUnrecorded))
	})

	t.Run("PS-6 chargeback vs live subscription is critical admin_pending", func(t *testing.T) {
		local := &fakeLocal{}
		sub := liveLocalSub(ProviderStripe, "sub_66")
		local.state.Subscriptions = []LocalSubscription{sub}
		withLiveEntitlement(local, &sub)
		subID := sub.ID
		local.payments = []LocalPayment{
			{ID: uuid.New(), MerchantSubjectID: sub.MerchantSubjectID, Processor: "stripe", TransactionID: "ch_66", AmountCents: 999, Status: "completed", SubscriptionID: &subID, PurchasedAt: testNow.Add(-5 * 24 * time.Hour)},
		}
		snap := &RemoteSnapshot{
			Provider:     ProviderStripe,
			Capabilities: Capabilities{Subscriptions: true, Transactions: true, Refunds: true, Chargebacks: true},
			Subscriptions: []RemoteSubscription{
				{ProcessorSubscriptionID: "sub_66", Status: SubscriptionStatusActive, NextBillingAt: tp(*sub.CurrentPeriodEndsAt)},
			},
			Transactions: []RemoteTransaction{
				{TransactionID: "dp_1", Type: TransactionTypeChargeback, Success: true, AmountCents: 999, OccurredAt: testNow.Add(-time.Hour), Raw: rawJSON(map[string]any{"charge": "ch_66"})},
			},
		}
		eng, _, writer := newTestEngine(ProviderStripe, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderStripe}})
		require.NoError(t, err)
		ps6 := findByType(res.Findings, FindingChargebackActiveSub)
		require.Len(t, ps6, 1)
		assert.Equal(t, SeverityCritical, ps6[0].Severity)
		assert.Equal(t, FindingStatusAdminPending, ps6[0].Status)
		assert.True(t, ps6[0].RequiresAdmin)
		assert.Zero(t, writer.totalCalls()) // never auto-applied
	})

	t.Run("PS-7 vault metadata mismatch adopts the processor record", func(t *testing.T) {
		local := &fakeLocal{}
		sub := liveLocalSub(ProviderNMI, "nmi-sub-7")
		pm := LocalPaymentMethod{
			ID: uuid.New(), MerchantSubjectID: sub.MerchantSubjectID, Processor: "nmi",
			VaultID: "vault-7", LastFour: "1111", ExpiryDate: "10/25",
		}
		local.state.Subscriptions = []LocalSubscription{sub}
		local.state.PaymentMethods = []LocalPaymentMethod{pm}
		snap := &RemoteSnapshot{
			Provider:     ProviderNMI,
			Capabilities: Capabilities{Subscriptions: true, Vault: true},
			Subscriptions: []RemoteSubscription{
				{ProcessorSubscriptionID: "nmi-sub-7", Status: SubscriptionStatusActive, NextBillingAt: tp(*sub.CurrentPeriodEndsAt)},
			},
			VaultEntries: []RemoteVaultEntry{
				{CustomerVaultID: "vault-7", CardLast4: "2222", CardExpiry: "1027"},
			},
		}
		eng, store, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)
		ps7 := findByType(res.Findings, FindingVaultMismatch)
		require.Len(t, ps7, 1)
		assert.Equal(t, "vault-7", ps7[0].SubjectKey)
		assert.Equal(t, 1, writer.calls["adopt_vault"])
		rec := store.record(ProviderNMI, FindingVaultMismatch, "vault-7")
		assert.Equal(t, FindingStatusAutoFixed, rec.Status)
		st, _ := local.Load(ctx, ProviderNMI)
		assert.Equal(t, "2222", st.PaymentMethods[0].LastFour)
	})

	t.Run("PS-8 duplicate live subscriptions for one subject are admin_pending", func(t *testing.T) {
		local := &fakeLocal{}
		sub := liveLocalSub(ProviderNMI, "dup-1")
		sub.TierGroup = "premium"
		dup := liveLocalSub(ProviderNMI, "dup-2")
		dup.MerchantSubjectID = sub.MerchantSubjectID
		dup.TierGroup = "premium"
		dup.Status = "past_due"
		local.state.Subscriptions = []LocalSubscription{sub, dup}
		snap := &RemoteSnapshot{
			Provider:     ProviderNMI,
			Capabilities: Capabilities{Subscriptions: true},
			Subscriptions: []RemoteSubscription{
				{ProcessorSubscriptionID: "dup-1", Status: SubscriptionStatusActive},
				{ProcessorSubscriptionID: "dup-2", Status: SubscriptionStatusActive},
			},
		}
		eng, _, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)
		ps8 := findByType(res.Findings, FindingDuplicateSubscriptions)
		require.Len(t, ps8, 1)
		assert.Equal(t, FindingStatusAdminPending, ps8[0].Status)
		assert.True(t, ps8[0].RequiresAdmin)
		assert.Zero(t, writer.calls["cancel"]) // duplicates are never auto-resolved
	})

	t.Run("PS-9 grants missing entitlements and revokes orphans", func(t *testing.T) {
		local := &fakeLocal{}
		missing := liveLocalSub(ProviderNMI, "ps9-grant")
		orphan := liveLocalSub(ProviderNMI, "ps9-revoke")
		orphan.Status = "cancelled"
		orphan.CancelType = "user"
		local.state.Subscriptions = []LocalSubscription{missing, orphan}
		withLiveEntitlement(local, &orphan) // cancelled sub still grants premium
		snap := &RemoteSnapshot{
			Provider:     ProviderNMI,
			Capabilities: Capabilities{Subscriptions: true},
			Subscriptions: []RemoteSubscription{
				{ProcessorSubscriptionID: "ps9-grant", Status: SubscriptionStatusActive, NextBillingAt: tp(*missing.CurrentPeriodEndsAt)},
			},
		}
		eng, store, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)

		ps9 := findByType(res.Findings, FindingEntitlementMismatch)
		require.Len(t, ps9, 2)
		assert.Equal(t, 1, writer.calls["grant"])
		assert.Equal(t, 1, writer.calls["revoke"])
		assert.Equal(t, FindingStatusAutoFixed, store.record(ProviderNMI, FindingEntitlementMismatch, missing.ID.String()).Status)
		assert.Equal(t, FindingStatusAutoFixed, store.record(ProviderNMI, FindingEntitlementMismatch, orphan.ID.String()).Status)

		st, _ := local.Load(ctx, ProviderNMI)
		require.Len(t, st.Entitlements, 1)
		assert.Equal(t, missing.ID, st.Entitlements[0].SourceID)
	})
}

// --- engine semantics ---------------------------------------------------------

func TestCircuitBreakerAbortsAbsenceBasedPS2(t *testing.T) {
	ctx := context.Background()
	local := &fakeLocal{}
	for i := 0; i < 20; i++ {
		local.state.Subscriptions = append(local.state.Subscriptions, liveLocalSub(ProviderNMI, fmt.Sprintf("nmi-%d", i)))
	}
	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		Capabilities: Capabilities{Subscriptions: true},
		Subscriptions: []RemoteSubscription{
			{ProcessorSubscriptionID: "nmi-0", Status: SubscriptionStatusActive},
		},
	}
	eng, store, writer := newTestEngine(ProviderNMI, snap, local)
	res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "circuit breaker")
	assert.Equal(t, "failed", res.Status)
	assert.True(t, res.Summary.Providers["nmi"].Aborted)
	assert.Zero(t, store.count(), "no findings may be persisted on a breaker abort")
	assert.Zero(t, writer.totalCalls())
}

func TestCircuitBreakerAllowsSmallRosters(t *testing.T) {
	ctx := context.Background()
	local := &fakeLocal{}
	for i := 0; i < 5; i++ { // below MinLocal: absence is honored
		local.state.Subscriptions = append(local.state.Subscriptions, liveLocalSub(ProviderNMI, fmt.Sprintf("nmi-%d", i)))
	}
	snap := &RemoteSnapshot{Provider: ProviderNMI, Capabilities: Capabilities{Subscriptions: true}}
	eng, _, _ := newTestEngine(ProviderNMI, snap, local)
	res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}})
	require.NoError(t, err)
	assert.Len(t, findByType(res.Findings, FindingLocalActiveRemoteDead), 5)
}

func TestIdentityStableAcrossRuns(t *testing.T) {
	ctx := context.Background()
	local := &fakeLocal{}
	sub := liveLocalSub(ProviderStripe, "sub_stable")
	local.state.Subscriptions = []LocalSubscription{sub}
	withLiveEntitlement(local, &sub)
	snap := &RemoteSnapshot{
		Provider:     ProviderStripe,
		Capabilities: Capabilities{Subscriptions: true},
		Subscriptions: []RemoteSubscription{
			{ProcessorSubscriptionID: "sub_stable", Status: SubscriptionStatusCancelled},
		},
	}
	eng, store, _ := newTestEngine(ProviderStripe, snap, local)

	res1, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderStripe}})
	require.NoError(t, err)
	require.Len(t, res1.Findings, 1)
	assert.Equal(t, 1, res1.Summary.Providers["stripe"].NewFindings)

	res2, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderStripe}})
	require.NoError(t, err)
	require.Len(t, res2.Findings, 1)
	assert.Equal(t, 0, res2.Summary.Providers["stripe"].NewFindings)
	assert.Equal(t, 1, res2.Summary.Providers["stripe"].UpdatedFindings)

	assert.Equal(t, 1, store.count(), "re-runs must update the standing finding, not duplicate it")
	rec := store.record(ProviderStripe, FindingLocalActiveRemoteDead, sub.ID.String())
	assert.Equal(t, 2, rec.OccurrenceCount)
	assert.Equal(t, res1.RunID, rec.FirstSeenRun)
	assert.Equal(t, res2.RunID, rec.LastSeenRun)
}

func TestAutoResolveOnDisappearance(t *testing.T) {
	ctx := context.Background()
	local := &fakeLocal{}
	sub := liveLocalSub(ProviderStripe, "sub_vanish")
	local.state.Subscriptions = []LocalSubscription{sub}
	deadSnap := &RemoteSnapshot{
		Provider:     ProviderStripe,
		Capabilities: Capabilities{Subscriptions: true},
		Subscriptions: []RemoteSubscription{
			{ProcessorSubscriptionID: "sub_vanish", Status: SubscriptionStatusCancelled},
		},
	}
	eng, store, _ := newTestEngine(ProviderStripe, deadSnap, local)
	_, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderStripe}})
	require.NoError(t, err)
	require.Equal(t, FindingStatusOpen, store.record(ProviderStripe, FindingLocalActiveRemoteDead, sub.ID.String()).Status)

	// The drift disappears (processor reactivated / first read was wrong).
	eng.Fetchers[ProviderStripe] = &fakeFetcher{provider: ProviderStripe, snap: &RemoteSnapshot{
		Provider:     ProviderStripe,
		Capabilities: Capabilities{Subscriptions: true},
		Subscriptions: []RemoteSubscription{
			{ProcessorSubscriptionID: "sub_vanish", Status: SubscriptionStatusActive, NextBillingAt: tp(*sub.CurrentPeriodEndsAt)},
		},
	}}
	res2, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderStripe}})
	require.NoError(t, err)

	rec := store.record(ProviderStripe, FindingLocalActiveRemoteDead, sub.ID.String())
	assert.Equal(t, FindingStatusResolved, rec.Status)
	assert.Equal(t, "auto_vanished", rec.Resolution)
	assert.GreaterOrEqual(t, res2.Summary.Providers["stripe"].AutoResolved, int64(1))
}

func TestIntentAnnotationForRecordedDelete(t *testing.T) {
	ctx := context.Background()
	local := &fakeLocal{}
	sub := liveLocalSub(ProviderNMI, "nmi-intent")
	sub.Status = "cancelled"
	sub.CancelType = "user"
	sub.CancelledAt = tp(testNow.Add(-24 * time.Hour))
	sub.DeletionScheduledAt = tp(testNow.Add(12 * time.Hour))
	local.state.Subscriptions = []LocalSubscription{sub}
	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		Capabilities: Capabilities{Subscriptions: true},
		Subscriptions: []RemoteSubscription{
			// The remote sub is still alive because the deferred delete has
			// not executed yet — exactly the recorded-intent case.
			{ProcessorSubscriptionID: "nmi-intent", Status: SubscriptionStatusActive},
		},
	}
	eng, _, writer := newTestEngine(ProviderNMI, snap, local)
	res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
	require.NoError(t, err)

	ps3 := findByType(res.Findings, FindingStatusMismatch)
	require.Len(t, ps3, 1)
	assert.Equal(t, FindingStatusOpen, ps3[0].Status, "intent-annotated drift must not escalate to the admin queue")
	assert.False(t, ps3[0].RequiresAdmin)
	require.NotNil(t, ps3[0].IntentEvidence)
	assert.Contains(t, ps3[0].IntentEvidence["explanation"], "intent executor")
	assert.Equal(t, deletionIntentAction, ps3[0].RecommendedAction)
	assert.Zero(t, writer.totalCalls(), "intent-annotated PS-3 is never auto-applied")
}

func TestCapabilityGating(t *testing.T) {
	ctx := context.Background()
	local := &fakeLocal{}
	sub := liveLocalSub(ProviderNMI, "nmi-gate")
	pm := LocalPaymentMethod{ID: uuid.New(), MerchantSubjectID: sub.MerchantSubjectID, Processor: "nmi", VaultID: "vault-gate", LastFour: "1111", ExpiryDate: "1025"}
	local.state.Subscriptions = []LocalSubscription{sub}
	local.state.PaymentMethods = []LocalPaymentMethod{pm}
	subID := sub.ID
	local.payments = []LocalPayment{
		{ID: uuid.New(), MerchantSubjectID: sub.MerchantSubjectID, Processor: "nmi", TransactionID: "txn-gate", AmountCents: 999, Status: "completed", SubscriptionID: &subID, PurchasedAt: testNow.Add(-time.Hour)},
	}
	// Snapshot contains a chargeback and a divergent vault entry, but the
	// provider declares neither capability: both checks must be skipped.
	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		Capabilities: Capabilities{Subscriptions: true, Transactions: true, Refunds: false, Chargebacks: false, Vault: false},
		Subscriptions: []RemoteSubscription{
			{ProcessorSubscriptionID: "nmi-gate", Status: SubscriptionStatusActive, NextBillingAt: tp(*sub.CurrentPeriodEndsAt)},
		},
		Transactions: []RemoteTransaction{
			{TransactionID: "cb-1", Type: TransactionTypeChargeback, Success: true, AmountCents: 999, OccurredAt: testNow.Add(-time.Hour), Raw: rawJSON(map[string]any{"order_id": sub.ID.String()})},
			{TransactionID: "rf-1", Type: TransactionTypeRefund, Success: true, AmountCents: 999, OccurredAt: testNow.Add(-time.Hour), Raw: rawJSON(map[string]any{"order_id": sub.ID.String()})},
		},
		VaultEntries: []RemoteVaultEntry{{CustomerVaultID: "vault-gate", CardLast4: "9999", CardExpiry: "1299"}},
	}
	eng, _, _ := newTestEngine(ProviderNMI, snap, local)
	res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}})
	require.NoError(t, err)
	withLiveEntitlement(local, &sub) // silence PS-9 noise after the fact (not asserted)

	assert.Empty(t, findByType(res.Findings, FindingChargebackActiveSub), "PS-6 must be capability-gated")
	assert.Empty(t, findByType(res.Findings, FindingRefundUnrecorded), "PS-5 must be capability-gated")
	assert.Empty(t, findByType(res.Findings, FindingVaultMismatch), "PS-7 must be capability-gated")
}

func TestEnforceIsIdempotent(t *testing.T) {
	ctx := context.Background()
	local := &fakeLocal{}
	dead := liveLocalSub(ProviderNMI, "nmi-idem")
	local.state.Subscriptions = []LocalSubscription{dead}
	withLiveEntitlement(local, &dead)
	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		Capabilities: Capabilities{Subscriptions: true},
		Subscriptions: []RemoteSubscription{
			{ProcessorSubscriptionID: "nmi-idem", Status: SubscriptionStatusExpired, RawStatus: "expired"},
		},
	}
	eng, store, writer := newTestEngine(ProviderNMI, snap, local)

	res1, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
	require.NoError(t, err)
	assert.Equal(t, 1, res1.Summary.Providers["nmi"].AutoFixed)
	firstCalls := writer.totalCalls()
	assert.Positive(t, firstCalls)

	// Second enforce run: local state already converged, so the diff is empty
	// and no write happens.
	res2, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
	require.NoError(t, err)
	assert.Empty(t, res2.Findings, "second enforce run must see a converged state")
	assert.Equal(t, 0, res2.Summary.Providers["nmi"].AutoFixed)
	assert.Equal(t, firstCalls, writer.totalCalls(), "second enforce run must be a write no-op")

	rec := store.record(ProviderNMI, FindingLocalActiveRemoteDead, dead.ID.String())
	assert.Equal(t, FindingStatusAutoFixed, rec.Status)
	assert.Equal(t, "enforced", rec.Resolution)
}

func TestAdvisoryNeverWrites(t *testing.T) {
	ctx := context.Background()
	local := &fakeLocal{}
	dead := liveLocalSub(ProviderNMI, "nmi-adv")
	local.state.Subscriptions = []LocalSubscription{dead}
	withLiveEntitlement(local, &dead)
	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		Capabilities: Capabilities{Subscriptions: true},
		Subscriptions: []RemoteSubscription{
			{ProcessorSubscriptionID: "nmi-adv", Status: SubscriptionStatusCancelled},
		},
	}
	eng, store, writer := newTestEngine(ProviderNMI, snap, local)
	res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}})
	require.NoError(t, err)
	require.NotEmpty(t, res.Findings)
	assert.Zero(t, writer.totalCalls(), "advisory mode performs zero local writes")
	rec := store.record(ProviderNMI, FindingLocalActiveRemoteDead, dead.ID.String())
	assert.Equal(t, FindingStatusOpen, rec.Status)
}

func TestEntitlementRevocationDisabledSkipsRevokes(t *testing.T) {
	ctx := context.Background()
	local := &fakeLocal{}
	orphan := liveLocalSub(ProviderNMI, "nmi-norevoke")
	orphan.Status = "cancelled"
	orphan.CancelType = "user"
	local.state.Subscriptions = []LocalSubscription{orphan}
	withLiveEntitlement(local, &orphan)
	snap := &RemoteSnapshot{Provider: ProviderNMI, Capabilities: Capabilities{Subscriptions: true}}
	eng, store, writer := newTestEngine(ProviderNMI, snap, local)
	eng.DisableEntitlementRevocation = true

	res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
	require.NoError(t, err)
	ps9 := findByType(res.Findings, FindingEntitlementMismatch)
	require.Len(t, ps9, 1)
	assert.Zero(t, writer.calls["revoke"], "revocation must be skipped when entitlement expiration is disabled")
	rec := store.record(ProviderNMI, FindingEntitlementMismatch, orphan.ID.String())
	assert.Equal(t, FindingStatusOpen, rec.Status, "skipped applies stay open")
	assert.Equal(t, 1, res.Summary.Providers["nmi"].ApplySkipped)
}

func TestDismissedFindingsStayDismissed(t *testing.T) {
	ctx := context.Background()
	local := &fakeLocal{}
	sub := liveLocalSub(ProviderStripe, "sub_dismiss")
	local.state.Subscriptions = []LocalSubscription{sub}
	withLiveEntitlement(local, &sub)
	snap := &RemoteSnapshot{
		Provider:     ProviderStripe,
		Capabilities: Capabilities{Subscriptions: true},
		Subscriptions: []RemoteSubscription{
			{ProcessorSubscriptionID: "sub_dismiss", Status: SubscriptionStatusCancelled},
		},
	}
	eng, store, writer := newTestEngine(ProviderStripe, snap, local)
	_, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderStripe}})
	require.NoError(t, err)

	rec := store.record(ProviderStripe, FindingLocalActiveRemoteDead, sub.ID.String())
	store.mu.Lock()
	rec.Status = FindingStatusDismissed
	store.mu.Unlock()

	_, err = eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderStripe}})
	require.NoError(t, err)
	rec = store.record(ProviderStripe, FindingLocalActiveRemoteDead, sub.ID.String())
	assert.Equal(t, FindingStatusDismissed, rec.Status)
	assert.Zero(t, writer.totalCalls(), "dismissed findings are never applied")
}

func TestDunningForensics(t *testing.T) {
	local := &fakeLocal{}
	never := liveLocalSub(ProviderNMI, "nmi-never")
	never.Status = "past_due"
	exhausted := liveLocalSub(ProviderNMI, "nmi-exhausted")
	exhausted.Status = "cancelled"
	exhausted.CancelType = "expired"
	exhausted.RetryAttempts = 3
	exhausted.LastRetryAt = tp(testNow.Add(-40 * 24 * time.Hour))
	local.state.Subscriptions = []LocalSubscription{never, exhausted}

	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		Capabilities: Capabilities{Subscriptions: true, Transactions: true},
		Transactions: []RemoteTransaction{
			nmiSaleTxn("d1", fmt.Sprintf("rebill-%s-%d", never.ID, testNow.Unix()), false),
			nmiSaleTxn("d2", fmt.Sprintf("rebill-%s-%d", never.ID, testNow.Unix()), false),
			nmiSaleTxn("d3", fmt.Sprintf("rebill-%s-%d", exhausted.ID, testNow.Unix()), false),
		},
	}
	report := computeDunningForensics(ProviderNMI, snap, &local.state, nil, "not configured", testNow)
	require.NotNil(t, report)
	assert.Equal(t, 2, report.SubscriptionsExamined)
	assert.Equal(t, 1, report.NeverAttempted)
	assert.Equal(t, 1, report.AttemptedExhausted)
	assert.Equal(t, map[string]int{"Insufficient funds": 3}, report.DeclineReasons)
	require.NotNil(t, report.LastLocalDunningAction)
	assert.True(t, report.LastLocalDunningAction.Equal(*exhausted.LastRetryAt))
	require.Len(t, report.Details, 2)
}

// --- PS-1 materialization (bootstrap mode v1.1) -------------------------------

// materializeFixture: a remote NMI subscription unknown locally whose identity
// (vault id -> local payment method) and plan (provider link on one price)
// both resolve, plus a successful remote charge attributable to it.
func materializeFixture() (*fakeLocal, *RemoteSnapshot, LocalPrice) {
	local := &fakeLocal{}
	subjectID := uuid.New()
	price := LocalPrice{
		ID:        uuid.New(),
		ProductID: uuid.New(),
		Amount:    999,
		Currency:  "usd",
		Status:    "active",
		Processors: map[string]map[string]string{
			"mobius": {"plan_id": "plan-gold"},
		},
	}
	local.state.Prices = []LocalPrice{price}
	local.state.PaymentMethods = []LocalPaymentMethod{{
		ID: uuid.New(), MerchantSubjectID: subjectID, Processor: "mobius",
		VaultID: "vault-77", LastFour: "1111", ExpiryDate: "1029",
	}}
	end := testNow.Add(20 * 24 * time.Hour)
	lastBilled := testNow.Add(-10 * 24 * time.Hour)
	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		Capabilities: Capabilities{Subscriptions: true, Transactions: true, Vault: true},
		Subscriptions: []RemoteSubscription{
			{
				ProcessorSubscriptionID: "remote-77",
				Status:                  SubscriptionStatusActive,
				CustomerID:              "vault-77",
				Email:                   "owner@example.com",
				PlanID:                  "plan-gold",
				NextBillingAt:           &end,
				LastBilledAt:            &lastBilled,
				AmountCents:             999,
				Currency:                "usd",
			},
		},
		Transactions: []RemoteTransaction{
			{
				TransactionID: "txn-mat-1", Type: TransactionTypeSale, Success: true,
				AmountCents: 999, Currency: "USD", OccurredAt: lastBilled,
				Raw: rawJSON(map[string]any{"customer_vault_id": "vault-77"}),
			},
		},
		VaultEntries: []RemoteVaultEntry{
			{CustomerVaultID: "vault-77", CardLast4: "1111", CardExpiry: "1029"},
		},
	}
	return local, snap, price
}

func TestMaterializePS1(t *testing.T) {
	ctx := context.Background()

	t.Run("advisory PS-1 stays admin_pending (advisory never writes)", func(t *testing.T) {
		local, snap, _ := materializeFixture()
		eng, store, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)
		ps1 := findByType(res.Findings, FindingRemoteSubMissingLocal)
		require.Len(t, ps1, 1)
		assert.Equal(t, FindingStatusAdminPending, ps1[0].Status)
		assert.True(t, ps1[0].RequiresAdmin)
		assert.Zero(t, writer.calls["materialize"])
		assert.Equal(t, FindingStatusAdminPending, store.record(ProviderNMI, FindingRemoteSubMissingLocal, "remote-77").Status)
	})

	t.Run("resolvable PS-1 materializes with payment + entitlements and resolves enforced", func(t *testing.T) {
		local, snap, price := materializeFixture()
		eng, store, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)

		ps1 := findByType(res.Findings, FindingRemoteSubMissingLocal)
		require.Len(t, ps1, 1)
		assert.Equal(t, 1, writer.calls["materialize"])

		rec := store.record(ProviderNMI, FindingRemoteSubMissingLocal, "remote-77")
		require.NotNil(t, rec)
		assert.Equal(t, FindingStatusAutoFixed, rec.Status)
		assert.Equal(t, "enforced", rec.Resolution)
		require.NotNil(t, rec.ResolutionEvid)
		assert.Equal(t, "vault_id", rec.ResolutionEvid["identity_via"])
		assert.Equal(t, price.ID.String(), rec.ResolutionEvid["price_id"])
		assert.Equal(t, true, rec.ResolutionEvid["payment_backfilled"])

		// The local subscription exists with remote status/periods…
		st, _ := local.Load(ctx, ProviderNMI)
		var created *LocalSubscription
		for i := range st.Subscriptions {
			if st.Subscriptions[i].ProcessorSubscriptionID == "remote-77" {
				created = &st.Subscriptions[i]
			}
		}
		require.NotNil(t, created)
		assert.Equal(t, "active", created.Status)
		assert.Equal(t, "mobius", created.Processor)
		require.NotNil(t, created.CurrentPeriodEndsAt)
		assert.True(t, created.CurrentPeriodEndsAt.Equal(*snap.Subscriptions[0].NextBillingAt))

		// …the snapshot charge is backfilled and entitlements granted.
		payments, _ := local.PaymentsByTransactionIDs(ctx, ProviderNMI, []string{"txn-mat-1"})
		require.Len(t, payments, 1)
		require.Len(t, st.Entitlements, 1)
		assert.Equal(t, created.ID, st.Entitlements[0].SourceID)

		// Re-run: converged, no duplicate, no second materialize write.
		res2, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)
		assert.Empty(t, findByType(res2.Findings, FindingRemoteSubMissingLocal))
		assert.Equal(t, 1, writer.calls["materialize"])
	})

	t.Run("ambiguous identity stays admin_pending with the blocker documented", func(t *testing.T) {
		local, snap, _ := materializeFixture()
		// Second subject shares the remote email -> two distinct candidates.
		other := liveLocalSub(ProviderNMI, "other-1")
		other.UserEmail = "owner@example.com"
		local.state.Subscriptions = append(local.state.Subscriptions, other)
		snap.Subscriptions = append(snap.Subscriptions, RemoteSubscription{
			ProcessorSubscriptionID: "other-1", Status: SubscriptionStatusActive,
		})
		eng, _, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)
		ps1 := findByType(res.Findings, FindingRemoteSubMissingLocal)
		require.Len(t, ps1, 1)
		assert.Equal(t, FindingStatusAdminPending, ps1[0].Status)
		assert.Contains(t, ps1[0].RemoteEvidence["materialize_blocked"], "ambiguous")
		assert.Zero(t, writer.calls["materialize"])
	})

	t.Run("unresolvable plan stays admin_pending", func(t *testing.T) {
		local, snap, _ := materializeFixture()
		local.state.Prices = nil // no provider link anywhere
		eng, _, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)
		ps1 := findByType(res.Findings, FindingRemoteSubMissingLocal)
		require.Len(t, ps1, 1)
		assert.Equal(t, FindingStatusAdminPending, ps1[0].Status)
		assert.Contains(t, ps1[0].RemoteEvidence["materialize_blocked"], "plan unresolved")
		assert.Zero(t, writer.calls["materialize"])
	})

	t.Run("remote past_due without a period end stays admin_pending", func(t *testing.T) {
		local, snap, _ := materializeFixture()
		snap.Subscriptions[0].Status = SubscriptionStatusPastDue
		snap.Subscriptions[0].NextBillingAt = nil
		eng, _, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)
		ps1 := findByType(res.Findings, FindingRemoteSubMissingLocal)
		require.Len(t, ps1, 1)
		assert.Equal(t, FindingStatusAdminPending, ps1[0].Status)
		assert.Contains(t, ps1[0].RemoteEvidence["materialize_blocked"], "past_due")
		assert.Zero(t, writer.calls["materialize"])
	})
}

// --- forensics: ClickHouse third evidence source -------------------------------

type fakeHistorySource struct {
	configured bool
	events     []HistoryEvent
	err        error
}

func (f *fakeHistorySource) Configured() bool { return f.configured }
func (f *fakeHistorySource) ListEvents(ctx context.Context, processors []string, since, until time.Time) ([]HistoryEvent, error) {
	return f.events, f.err
}

func TestDunningForensicsHistorySource(t *testing.T) {
	ctx := context.Background()

	newFixture := func() (*fakeLocal, *RemoteSnapshot, LocalSubscription) {
		local := &fakeLocal{}
		sub := liveLocalSub(ProviderNMI, "nmi-hist")
		sub.Status = "past_due"
		local.state.Subscriptions = []LocalSubscription{sub}
		snap := &RemoteSnapshot{
			Provider:     ProviderNMI,
			Capabilities: Capabilities{Subscriptions: true, Transactions: true},
			Subscriptions: []RemoteSubscription{
				{ProcessorSubscriptionID: "nmi-hist", Status: SubscriptionStatusPastDue, NextBillingAt: tp(*sub.CurrentPeriodEndsAt)},
			},
		}
		return local, snap, sub
	}

	t.Run("history events merge into the per-subscription timeline and aggregates", func(t *testing.T) {
		local, snap, sub := newFixture()
		subID := sub.ID
		histAt := testNow.Add(-200 * 24 * time.Hour) // deep history the provider window can't see
		eng, _, _ := newTestEngine(ProviderNMI, snap, local)
		eng.History = &fakeHistorySource{configured: true, events: []HistoryEvent{
			{Table: "payment_events", EventType: "charge_failed", Processor: "nmi", SubscriptionID: &subID, OccurredAt: histAt},
			{Table: "payment_events", EventType: "charge_success", Processor: "nmi", SubscriptionID: &subID, OccurredAt: histAt.Add(-30 * 24 * time.Hour)},
			{Table: "subscription_events", EventType: "subscription_cancelled", Processor: "nmi", ProcessorSubscriptionID: "nmi-hist", OccurredAt: histAt.Add(24 * time.Hour)},
			{Table: "payment_events", EventType: "charge_failed", Processor: "nmi", OccurredAt: histAt}, // uncorrelated: no ids
		}}
		res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)

		d := res.Summary.Providers["nmi"].Dunning
		require.NotNil(t, d)
		assert.Equal(t, "ok: 4 events (3 correlated)", d.HistorySource)
		require.Len(t, d.Details, 1)
		line := d.Details[0]
		assert.Equal(t, 3, line.HistoryEvents)
		assert.Equal(t, 1, line.HistoryFailures)
		assert.Equal(t, 1, line.HistorySuccesses)

		// Timeline carries source-tagged entries from history.
		var sources, kinds []string
		for _, ev := range line.Timeline {
			sources = append(sources, ev.Source)
			kinds = append(kinds, ev.Kind)
		}
		assert.Contains(t, sources, "history")
		assert.Contains(t, kinds, "charge_failed")
		assert.Contains(t, kinds, "subscription_cancelled")

		// "Last dunning action per ANY source": only history acted here.
		require.NotNil(t, d.LastDunningActionAnySource)
		assert.Equal(t, "history", d.LastDunningActionVia)
		assert.True(t, d.LastDunningActionAnySource.Equal(histAt))

		// History failures classify a frozen-retry sub as never_attempted even
		// with zero provider declines in the window.
		assert.Equal(t, "never_attempted", line.Classification)
		assert.Equal(t, 1, d.NeverAttempted)
	})

	t.Run("unreachable ClickHouse degrades to a note, never an error", func(t *testing.T) {
		local, snap, _ := newFixture()
		eng, _, _ := newTestEngine(ProviderNMI, snap, local)
		eng.History = &fakeHistorySource{configured: true, err: fmt.Errorf("dial tcp: connection refused")}
		res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err, "history source failure must not fail the run")
		d := res.Summary.Providers["nmi"].Dunning
		require.NotNil(t, d)
		assert.Contains(t, d.HistorySource, "unavailable: dial tcp")
		assert.Equal(t, "completed", res.Status)
	})

	t.Run("unconfigured source is noted", func(t *testing.T) {
		local, snap, _ := newFixture()
		eng, _, _ := newTestEngine(ProviderNMI, snap, local)
		eng.History = &fakeHistorySource{configured: false}
		res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}})
		require.NoError(t, err)
		d := res.Summary.Providers["nmi"].Dunning
		require.NotNil(t, d)
		assert.Contains(t, d.HistorySource, "not configured")
	})
}

func TestParseRebillOrderID(t *testing.T) {
	id := uuid.New()
	got, ok := parseRebillOrderID(fmt.Sprintf("rebill-%s-1765432100", id))
	require.True(t, ok)
	assert.Equal(t, id, got)

	got, ok = parseRebillOrderID(id.String())
	require.True(t, ok)
	assert.Equal(t, id, got)

	_, ok = parseRebillOrderID("rebill-not-a-uuid-123")
	assert.False(t, ok)
	_, ok = parseRebillOrderID("")
	assert.False(t, ok)
	_, ok = parseRebillOrderID("upgrade-12345678-87654321")
	assert.False(t, ok)
}
