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

// localEntitlement is TEST-ONLY bookkeeping for the fake writer's entitlement
// grant/revoke side effects (the PULL engine no longer loads entitlements —
// the DERIVE converge pass owns that check, #665).
type localEntitlement struct {
	ID          uuid.UUID
	CustomerID  uuid.UUID
	Entitlement string
	SourceID    uuid.UUID
	StartAt     time.Time
	EndAt       *time.Time
}

// fakeLocal serves LocalState and the payment lookup from in-memory slices;
// the fakeWriter mutates the same slices so enforce convergence is visible to
// the next run.
type fakeLocal struct {
	mu       sync.Mutex
	state    LocalState
	ents     []localEntitlement
	payments []LocalPayment
}

func (l *fakeLocal) Load(ctx context.Context, provider Provider, _ uuid.UUID) (*LocalState, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := LocalState{
		Subscriptions:  append([]LocalSubscription(nil), l.state.Subscriptions...),
		PaymentMethods: append([]LocalPaymentMethod(nil), l.state.PaymentMethods...),
		Prices:         append([]LocalPrice(nil), l.state.Prices...),
	}
	return &cp, nil
}

func (l *fakeLocal) entsSnapshot() []localEntitlement {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]localEntitlement(nil), l.ents...)
}

func (l *fakeLocal) PaymentsByTransactionIDs(ctx context.Context, provider Provider, _ uuid.UUID, ids []string) ([]LocalPayment, error) {
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
		rid := runID
		rec = &FindingRecord{
			ID: uuid.New(), Provider: f.Provider, Type: f.Type, SubjectKey: f.SubjectKey,
			FirstSeenRun: &rid, CreatedAt: now,
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
	if rec.Status != FindingStatusIgnored {
		rec.Status = f.Status
		rec.ResolvedAt = nil
		rec.Resolution = ""
		rec.ResolutionEvid = nil
	}
	lsr := runID
	rec.LastSeenRun = &lsr
	rec.LastSeenAt = now
	rec.UpdatedAt = now
	return *rec, nil
}

func (s *memStore) ListActionableFindingsByProvider(ctx context.Context, provider Provider) ([]FindingRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []FindingRecord
	for _, rec := range s.findings {
		if rec.Provider == provider && (rec.Status == FindingStatusReconcileRequired || rec.Status == FindingStatusAdminRequired) {
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
		if rec.Status != FindingStatusReconcileRequired && rec.Status != FindingStatusAdminRequired {
			continue
		}
		if rec.LastSeenRun != nil && *rec.LastSeenRun == runID {
			continue
		}
		rec.Status = FindingStatusFixed
		rec.Resolution = "auto_vanished"
		rec.ResolvedAt = &now
		rec.NotifiedAt = nil
		rec.NotifiedSeverity = ""
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
	rec.Status = FindingStatusFixed
	rec.Resolution = "auto_vanished"
	rec.ResolvedAt = &now
	rec.NotifiedAt = nil
	rec.NotifiedSeverity = ""
	return nil
}

func (s *memStore) MarkFindingNotified(ctx context.Context, id uuid.UUID, at time.Time, severity Severity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[id]
	if !ok {
		return fmt.Errorf("no finding %s", id)
	}
	t := at
	rec.NotifiedAt = &t
	rec.NotifiedSeverity = string(severity)
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
	rec.NotifiedAt = nil
	rec.NotifiedSeverity = ""
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

// fakeDecisions applies decider transitions onto the fakeLocal state,
// mirroring reconcile.ApplyDecision's guards. It SHARES the fakeWriter's call
// counter so totalCalls() still means "any enforce write happened".
type fakeDecisions struct {
	local *fakeLocal
	calls map[string]int
}

func (fd *fakeDecisions) ApplyDecision(ctx context.Context, subscriptionID uuid.UUID, d Decision) (bool, error) {
	fd.local.mu.Lock()
	defer fd.local.mu.Unlock()
	var s *LocalSubscription
	for i := range fd.local.state.Subscriptions {
		if fd.local.state.Subscriptions[i].ID == subscriptionID {
			s = &fd.local.state.Subscriptions[i]
			break
		}
	}
	if s == nil {
		return false, fmt.Errorf("no subscription %s", subscriptionID)
	}
	transitional := s.Status == "active" || s.Status == "past_due" || s.Status == "unknown"
	switch d.Kind {
	case TransitionParkUnknown:
		if s.Status != "active" && s.Status != "past_due" {
			return false, nil
		}
		fd.calls["park"]++
		s.Status = "unknown"
		return true, nil
	case TransitionPastDue:
		if !transitional || s.Status == "past_due" {
			return false, nil
		}
		fd.calls["past_due"]++
		s.Status = "past_due"
		return true, nil
	case TransitionRenew, TransitionAdoptPeriodEnd:
		if !transitional {
			return false, nil
		}
		fd.calls["adopt_status"]++
		if d.Kind == TransitionRenew && d.NewPeriodEnd != nil {
			s.CurrentPeriodStartsAt = s.CurrentPeriodEndsAt
		}
		s.Status = "active"
		if d.NewPeriodEnd != nil {
			s.CurrentPeriodEndsAt = d.NewPeriodEnd
		}
		return true, nil
	case TransitionCancel:
		if !transitional {
			return false, nil
		}
		fd.calls["cancel"]++
		now := time.Now()
		s.Status = "cancelled"
		s.CancelType = "expired"
		s.CancelledAt = &now
		// ResolveCancelled revokes subscription-sourced entitlements.
		var kept []localEntitlement
		for _, ent := range fd.local.ents {
			if ent.SourceID == s.ID {
				fd.calls["revoke"]++
				continue
			}
			kept = append(kept, ent)
		}
		fd.local.ents = kept
		return true, nil
	default:
		return false, nil
	}
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
		ID: uuid.New(), CustomerID: a.CustomerID, Rail: a.Rail,
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
		ID: uuid.New(), CustomerID: a.CustomerID, Rail: a.Rail,
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

func (w *fakeWriter) AdoptPaymentMethod(ctx context.Context, a AdoptPaymentMethodAction) (bool, error) {
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
		for _, ent := range w.local.ents {
			if ent.SourceID == a.SubscriptionID && ent.Entitlement == name {
				exists = true
				break
			}
		}
		if exists {
			continue
		}
		w.local.ents = append(w.local.ents, localEntitlement{
			ID: uuid.New(), CustomerID: a.CustomerID, Entitlement: name,
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
		if w.local.state.Subscriptions[i].RailSubscriptionID == a.RailSubscriptionID {
			w.local.mu.Unlock()
			return MaterializeResult{}, nil // already materialized
		}
	}
	priceID := a.PriceID
	sub := LocalSubscription{
		ID:                    uuid.New(),
		CustomerID:            a.CustomerID,
		PriceID:               &priceID,
		ProductID:             a.ProductID,
		Status:                a.Status,
		Rail:                  a.Rail,
		RailSubscriptionID:    a.RailSubscriptionID,
		UserEmail:             a.UserEmail,
		CurrentPeriodStartsAt: a.PeriodStartsAt,
		CurrentPeriodEndsAt:   a.PeriodEndsAt,
		StartedAt:             testNow,
		EntitlementNames:      fakeMaterializeEntitlements,
	}
	w.local.state.Subscriptions = append(w.local.state.Subscriptions, sub)
	w.local.mu.Unlock()

	res := MaterializeResult{SubscriptionID: sub.ID, Created: true}
	// testNow, not time.Now() — a real clock makes the fixture date-dependent.
	if a.PeriodEndsAt == nil || a.PeriodEndsAt.After(testNow) {
		start := testNow
		if a.PeriodStartsAt != nil {
			start = *a.PeriodStartsAt
		}
		granted, err := w.GrantEntitlements(ctx, GrantEntitlementsAction{
			SubscriptionID: sub.ID,
			CustomerID:     a.CustomerID,
			Entitlements:   fakeMaterializeEntitlements,
			StartAt:        start,
			EndAt:          a.PeriodEndsAt,
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

// --- helpers ----------------------------------------------------------------

func tp(t time.Time) *time.Time { return &t }

var testNow = time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)

// fakeRunRecorder is the in-memory DestructiveRunRecorder these unit tests need
// (or#859: an enforce pass with no run record REFUSES). It records nothing that
// is asserted on here — the reversal itself is proven against Postgres in
// converge_rollback_integration_test.go.
type fakeRunRecorder struct {
	opened   int
	captured int
	finished []string
}

func (r *fakeRunRecorder) Open(context.Context, OpenDestructiveRunParams) (uuid.UUID, error) {
	r.opened++
	return uuid.New(), nil
}

func (r *fakeRunRecorder) CaptureSubscription(context.Context, uuid.UUID, uuid.UUID) (time.Time, error) {
	r.captured++
	return testNow, nil
}

func (r *fakeRunRecorder) StampIntents(context.Context, uuid.UUID, uuid.UUID, time.Time) (int, error) {
	return 0, nil
}

func (r *fakeRunRecorder) Finish(_ context.Context, _ uuid.UUID, status string, _ map[string]int) error {
	r.finished = append(r.finished, status)
	return nil
}

func newTestEngine(provider Provider, snap *RemoteSnapshot, local *fakeLocal) (*Engine, *memStore, *fakeWriter) {
	store := newMemStore()
	writer := newFakeWriter(local)
	eng := &Engine{
		Fetchers:  map[Provider]RailFetcher{provider: &fakeFetcher{provider: provider, snap: snap}},
		Store:     store,
		Local:     local,
		Writer:    writer,
		Decisions: &fakeDecisions{local: local, calls: writer.calls},
		Runs:      &fakeRunRecorder{},
		Now:       func() time.Time { return testNow },
	}
	return eng, store, writer
}

func liveLocalSub(provider Provider, psid string) LocalSubscription {
	priceID := uuid.New()
	return LocalSubscription{
		ID:                    uuid.New(),
		CustomerID:            uuid.New(),
		PriceID:               &priceID,
		ProductID:             uuid.New(),
		Status:                "active",
		Rail:                  string(provider),
		RailSubscriptionID:    psid,
		CurrentPeriodStartsAt: tp(testNow.Add(-10 * 24 * time.Hour)),
		CurrentPeriodEndsAt:   tp(testNow.Add(20 * 24 * time.Hour)),
		StartedAt:             testNow.Add(-100 * 24 * time.Hour),
		EntitlementNames:      []string{"premium"},
	}
}

func withLiveEntitlement(local *fakeLocal, s *LocalSubscription) {
	local.ents = append(local.ents, localEntitlement{
		ID: uuid.New(), CustomerID: s.CustomerID, Entitlement: "premium",
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

	t.Run("PS-1 rail sub missing locally is critical requires_review with no apply", func(t *testing.T) {
		local := &fakeLocal{}
		known := liveLocalSub(ProviderNMI, "known-1")
		local.state.Subscriptions = []LocalSubscription{known}
		withLiveEntitlement(local, &known)
		snap := &RemoteSnapshot{
			Provider:     ProviderNMI,
			Capabilities: Capabilities{Subscriptions: true},
			Subscriptions: []RemoteSubscription{
				{RailSubscriptionID: "known-1", Status: SubscriptionStatusActive, NextBillingAt: tp(*known.CurrentPeriodEndsAt)},
				{RailSubscriptionID: "ghost-7", Status: SubscriptionStatusActive, Email: "ghost@example.com"},
			},
		}
		eng, store, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
		require.NoError(t, err)

		ps1 := findByType(res.Findings, FindingRemoteSubMissingLocal)
		require.Len(t, ps1, 1)
		assert.Equal(t, "ghost-7", ps1[0].SubjectKey)
		assert.Equal(t, SeverityCritical, ps1[0].Severity)
		assert.Equal(t, FindingStatusAdminRequired, ps1[0].Status)
		assert.True(t, ps1[0].RequiresAdmin)
		// Never auto-created, even in enforce mode.
		assert.Equal(t, FindingStatusAdminRequired, store.record(ProviderNMI, FindingRemoteSubMissingLocal, "ghost-7").Status)
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
				{RailSubscriptionID: "known-1", Status: SubscriptionStatusActive},
				{RailSubscriptionID: "ghost-9", Status: SubscriptionStatusActive, Email: "JANE@example.com"},
			},
		}
		eng, _, _ := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
		require.NoError(t, err)
		ps1 := findByType(res.Findings, FindingRemoteSubMissingLocal)
		require.Len(t, ps1, 1)
		require.NotNil(t, ps1[0].LocalEvidence)
		candidates, ok := ps1[0].LocalEvidence["email_candidates"].([]map[string]any)
		require.True(t, ok)
		require.Len(t, candidates, 1)
		assert.Equal(t, known.ID.String(), candidates[0]["subscription_id"])
		assert.Equal(t, FindingStatusAdminRequired, ps1[0].Status)
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
			// #842: only a roster that PROVES it covered everything makes
			// absence actionable.
			Coverage: SnapshotCoverage{SubscriptionsExhaustive: true},
			Subscriptions: []RemoteSubscription{
				{RailSubscriptionID: "alive-1", Status: SubscriptionStatusActive, NextBillingAt: tp(*alive.CurrentPeriodEndsAt)},
			},
		}
		eng, store, writer := newTestEngine(ProviderNMI, snap, local)
		// 1 of 2 remote-live clears the ratio breaker, 1 cancellation clears the
		// per-pass cap.
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
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
		st, _ := local.Load(ctx, ProviderNMI, uuid.Nil)
		for _, s := range st.Subscriptions {
			if s.ID == dead.ID {
				assert.Equal(t, "cancelled", s.Status)
				assert.Equal(t, "expired", s.CancelType)
			}
		}
		for _, ent := range local.entsSnapshot() {
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
				{RailSubscriptionID: "sub_123", Status: SubscriptionStatusCancelled, RawStatus: "canceled"},
			},
		}
		eng, _, _ := newTestEngine(ProviderStripe, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderStripe}, PSPs: testPSPs(ProviderStripe)})
		require.NoError(t, err)
		ps2 := findByType(res.Findings, FindingLocalActiveRemoteDead)
		require.Len(t, ps2, 1)
		assert.Equal(t, FindingStatusReconcileRequired, ps2[0].Status)
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
		res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderCCBill}, PSPs: testPSPs(ProviderCCBill)})
		require.NoError(t, err)
		assert.Empty(t, findByType(res.Findings, FindingLocalActiveRemoteDead))
	})

	t.Run("PS-3 adopts rail status and periods on enforce", func(t *testing.T) {
		local := &fakeLocal{}
		sub := liveLocalSub(ProviderStripe, "sub_42")
		sub.Status = "past_due"
		local.state.Subscriptions = []LocalSubscription{sub}
		remoteEnd := testNow.Add(25 * 24 * time.Hour)
		snap := &RemoteSnapshot{
			Provider:     ProviderStripe,
			Capabilities: Capabilities{Subscriptions: true},
			Subscriptions: []RemoteSubscription{
				{RailSubscriptionID: "sub_42", Status: SubscriptionStatusActive, RawStatus: "active",
					LastBilledAt: tp(testNow.Add(-5 * 24 * time.Hour)), NextBillingAt: &remoteEnd},
			},
		}
		eng, store, writer := newTestEngine(ProviderStripe, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderStripe}, PSPs: testPSPs(ProviderStripe)})
		require.NoError(t, err)
		ps3 := findByType(res.Findings, FindingStatusMismatch)
		require.Len(t, ps3, 1)
		require.Len(t, res.PlannedChanges, 1)
		assert.Equal(t, "planned", res.PlannedChanges[0].Phase)
		assert.Equal(t, "subscriptions", res.PlannedChanges[0].Table)
		assert.Equal(t, "update", res.PlannedChanges[0].Operation)
		assert.Equal(t, sub.ID.String(), res.PlannedChanges[0].RowID)
		require.Len(t, res.AppliedChanges, 1)
		assert.Equal(t, "applied", res.AppliedChanges[0].Phase)
		assert.Equal(t, "subscriptions", res.AppliedChanges[0].Table)
		assert.Equal(t, "update", res.AppliedChanges[0].Operation)
		assert.Equal(t, sub.ID.String(), res.AppliedChanges[0].RowID)
		assert.Equal(t, 1, res.AppliedChanges[0].RowsAffected)
		assert.Equal(t, 1, writer.calls["adopt_status"])
		rec := store.record(ProviderStripe, FindingStatusMismatch, sub.ID.String())
		assert.Equal(t, FindingStatusAutoFixed, rec.Status)

		st, _ := local.Load(ctx, ProviderStripe, uuid.Nil)
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
				{RailSubscriptionID: "sub_dead", Status: SubscriptionStatusActive},
			},
		}
		eng, _, writer := newTestEngine(ProviderStripe, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderStripe}, PSPs: testPSPs(ProviderStripe)})
		require.NoError(t, err)
		ps3 := findByType(res.Findings, FindingStatusMismatch)
		require.Len(t, ps3, 1)
		assert.Equal(t, FindingStatusAdminRequired, ps3[0].Status)
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
				{RailSubscriptionID: "nmi-sub-1", Status: SubscriptionStatusActive, NextBillingAt: tp(*sub.CurrentPeriodEndsAt)},
			},
			Transactions: []RemoteTransaction{
				nmiSaleTxn("txn-1001", fmt.Sprintf("rebill-%s-%d", sub.ID, testNow.Unix()), true),
			},
		}
		eng, store, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
		require.NoError(t, err)

		ps4 := findByType(res.Findings, FindingChargeMissingLocal)
		require.Len(t, ps4, 1)
		assert.Equal(t, "txn-1001", ps4[0].SubjectKey)
		assert.Equal(t, SeverityHigh, ps4[0].Severity)
		assert.Equal(t, 1, writer.calls["backfill_payment"])
		rec := store.record(ProviderNMI, FindingChargeMissingLocal, "txn-1001")
		assert.Equal(t, FindingStatusAutoFixed, rec.Status)

		// Payment exists + entitlement granted for the current period.
		payments, _ := local.PaymentsByTransactionIDs(ctx, ProviderNMI, uuid.Nil, []string{"txn-1001"})
		require.Len(t, payments, 1)
		assert.Equal(t, sub.CustomerID, payments[0].CustomerID)
		ents := local.entsSnapshot()
		require.Len(t, ents, 1)
		assert.Equal(t, "premium", ents[0].Entitlement)
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
				{RailSubscriptionID: "nmi-a", Status: SubscriptionStatusActive},
				{RailSubscriptionID: "nmi-b", Status: SubscriptionStatusActive},
			},
			Transactions: []RemoteTransaction{txn},
		}
		eng, _, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
		require.NoError(t, err)
		ps4 := findByType(res.Findings, FindingChargeMissingLocal)
		require.Len(t, ps4, 1)
		assert.Equal(t, FindingStatusAdminRequired, ps4[0].Status)
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
			ID: uuid.New(), CustomerID: sub.CustomerID, Rail: "nmi",
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
				{RailSubscriptionID: "nmi-sub-5", Status: SubscriptionStatusActive, NextBillingAt: tp(*sub.CurrentPeriodEndsAt)},
			},
			Transactions: []RemoteTransaction{refund},
		}
		eng, store, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
		require.NoError(t, err)

		ps5 := findByType(res.Findings, FindingRefundUnrecorded)
		require.Len(t, ps5, 1)
		assert.Equal(t, 1, writer.calls["record_refund"])
		rec := store.record(ProviderNMI, FindingRefundUnrecorded, "txn-5001")
		assert.Equal(t, FindingStatusAutoFixed, rec.Status)
		payments, _ := local.PaymentsByTransactionIDs(ctx, ProviderNMI, uuid.Nil, []string{"txn-5001"})
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
			{ID: originalID, CustomerID: sub.CustomerID, Rail: "stripe", TransactionID: "ch_1", AmountCents: 999, Status: "refunded", SubscriptionID: &subID, PurchasedAt: testNow.Add(-9 * 24 * time.Hour)},
			{ID: uuid.New(), CustomerID: sub.CustomerID, Rail: "stripe", TransactionID: "re_1", AmountCents: -999, Status: "completed", SubscriptionID: &subID, RefundedPaymentID: &originalID, PurchasedAt: testNow.Add(-8 * 24 * time.Hour)},
		}
		snap := &RemoteSnapshot{
			Provider:     ProviderStripe,
			Capabilities: Capabilities{Subscriptions: true, Transactions: true, Refunds: true, Chargebacks: true},
			Subscriptions: []RemoteSubscription{
				{RailSubscriptionID: "sub_55", Status: SubscriptionStatusActive, NextBillingAt: tp(*sub.CurrentPeriodEndsAt)},
			},
			Transactions: []RemoteTransaction{
				{TransactionID: "re_1", Type: TransactionTypeRefund, Success: true, AmountCents: 999, OccurredAt: testNow.Add(-8 * 24 * time.Hour), Raw: rawJSON(map[string]any{"charge": "ch_1"})},
			},
		}
		eng, _, _ := newTestEngine(ProviderStripe, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderStripe}, PSPs: testPSPs(ProviderStripe)})
		require.NoError(t, err)
		assert.Empty(t, findByType(res.Findings, FindingRefundUnrecorded))
	})

	t.Run("PS-6 chargeback vs live subscription is critical requires_review", func(t *testing.T) {
		local := &fakeLocal{}
		sub := liveLocalSub(ProviderStripe, "sub_66")
		local.state.Subscriptions = []LocalSubscription{sub}
		withLiveEntitlement(local, &sub)
		subID := sub.ID
		local.payments = []LocalPayment{
			{ID: uuid.New(), CustomerID: sub.CustomerID, Rail: "stripe", TransactionID: "ch_66", AmountCents: 999, Status: "completed", SubscriptionID: &subID, PurchasedAt: testNow.Add(-5 * 24 * time.Hour)},
		}
		snap := &RemoteSnapshot{
			Provider:     ProviderStripe,
			Capabilities: Capabilities{Subscriptions: true, Transactions: true, Refunds: true, Chargebacks: true},
			Subscriptions: []RemoteSubscription{
				{RailSubscriptionID: "sub_66", Status: SubscriptionStatusActive, NextBillingAt: tp(*sub.CurrentPeriodEndsAt)},
			},
			Transactions: []RemoteTransaction{
				{TransactionID: "dp_1", Type: TransactionTypeChargeback, Success: true, AmountCents: 999, OccurredAt: testNow.Add(-time.Hour), Raw: rawJSON(map[string]any{"charge": "ch_66"})},
			},
		}
		eng, _, writer := newTestEngine(ProviderStripe, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderStripe}, PSPs: testPSPs(ProviderStripe)})
		require.NoError(t, err)
		ps6 := findByType(res.Findings, FindingChargebackActiveSub)
		require.Len(t, ps6, 1)
		assert.Equal(t, SeverityCritical, ps6[0].Severity)
		assert.Equal(t, FindingStatusAdminRequired, ps6[0].Status)
		assert.True(t, ps6[0].RequiresAdmin)
		assert.Zero(t, writer.totalCalls()) // never auto-applied
	})

	t.Run("PS-7 vault metadata mismatch adopts the rail record", func(t *testing.T) {
		local := &fakeLocal{}
		sub := liveLocalSub(ProviderNMI, "nmi-sub-7")
		pm := LocalPaymentMethod{
			ID: uuid.New(), CustomerID: sub.CustomerID, Rail: "nmi",
			RailCustomerRef: "vault-7", LastFour: "1111", ExpiryDate: "10/25",
		}
		local.state.Subscriptions = []LocalSubscription{sub}
		local.state.PaymentMethods = []LocalPaymentMethod{pm}
		snap := &RemoteSnapshot{
			Provider:     ProviderNMI,
			Capabilities: Capabilities{Subscriptions: true, Vault: true},
			Subscriptions: []RemoteSubscription{
				{RailSubscriptionID: "nmi-sub-7", Status: SubscriptionStatusActive, NextBillingAt: tp(*sub.CurrentPeriodEndsAt)},
			},
			PaymentMethods: []RemotePaymentMethod{
				{RailCustomerRef: "vault-7", CardLast4: "2222", CardExpiry: "1027"},
			},
		}
		eng, store, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
		require.NoError(t, err)
		ps7 := findByType(res.Findings, FindingPaymentMethodMismatch)
		require.Len(t, ps7, 1)
		assert.Equal(t, "vault-7", ps7[0].SubjectKey)
		assert.Equal(t, 1, writer.calls["adopt_vault"])
		rec := store.record(ProviderNMI, FindingPaymentMethodMismatch, "vault-7")
		assert.Equal(t, FindingStatusAutoFixed, rec.Status)
		st, _ := local.Load(ctx, ProviderNMI, uuid.Nil)
		assert.Equal(t, "2222", st.PaymentMethods[0].LastFour)
	})

	t.Run("PS-8 duplicate live subscriptions for one subject are requires_review", func(t *testing.T) {
		local := &fakeLocal{}
		sub := liveLocalSub(ProviderNMI, "dup-1")
		sub.TierGroup = "premium"
		dup := liveLocalSub(ProviderNMI, "dup-2")
		dup.CustomerID = sub.CustomerID
		dup.TierGroup = "premium"
		dup.Status = "past_due"
		local.state.Subscriptions = []LocalSubscription{sub, dup}
		snap := &RemoteSnapshot{
			Provider:     ProviderNMI,
			Capabilities: Capabilities{Subscriptions: true},
			Subscriptions: []RemoteSubscription{
				{RailSubscriptionID: "dup-1", Status: SubscriptionStatusActive},
				{RailSubscriptionID: "dup-2", Status: SubscriptionStatusActive},
			},
		}
		eng, _, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
		require.NoError(t, err)
		ps8 := findByType(res.Findings, FindingDuplicateSubscriptions)
		require.Len(t, ps8, 1)
		assert.Equal(t, FindingStatusAdminRequired, ps8[0].Status)
		assert.True(t, ps8[0].RequiresAdmin)
		assert.Zero(t, writer.calls["cancel"]) // duplicates are never auto-resolved
	})

	// PS-9 (subscription ↔ entitlement drift) moved to the Convergence Engine's
	// DERIVE pass (#665) — see converge_derive_mismatch_integration_test.go.
}

// --- engine semantics ---------------------------------------------------------

// #665: a completed provider section's coverage is a pull proof; failed or
// breaker-aborted sections prove nothing (their coverage must never feed the
// confirmed-absence gate).
func TestPullProofsOnlyFromCompletedProviders(t *testing.T) {
	ctx := context.Background()
	local := &fakeLocal{}
	sub := liveLocalSub(ProviderNMI, "nmi-proof")
	local.state.Subscriptions = []LocalSubscription{sub}
	okSnap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		Capabilities: Capabilities{Subscriptions: true},
		Subscriptions: []RemoteSubscription{
			{RailSubscriptionID: "nmi-proof", Status: SubscriptionStatusActive, NextBillingAt: tp(*sub.CurrentPeriodEndsAt)},
		},
		Coverage: SnapshotCoverage{SubscriptionsExhaustive: true},
	}
	eng, _, _ := newTestEngine(ProviderNMI, okSnap, local)
	eng.Fetchers[ProviderStripe] = &fakeFetcher{provider: ProviderStripe, err: fmt.Errorf("stripe down")}

	res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, PSPs: testPSPs(ProviderNMI, ProviderStripe, ProviderCCBill, ProviderSolana)})
	require.Error(t, err, "the failed provider fails the run")
	require.NotNil(t, res)

	proofs := res.PullProofs()
	require.Len(t, proofs, 1, "only the completed provider proves anything")
	assert.True(t, proofs[ProviderNMI].Coverage.SubscriptionsExhaustive)
	_, hasStripe := proofs[ProviderStripe]
	assert.False(t, hasStripe)
}

func TestCircuitBreakerAbortsAbsenceBasedPS2(t *testing.T) {
	ctx := context.Background()
	local := &fakeLocal{}
	for i := 0; i < 20; i++ {
		local.state.Subscriptions = append(local.state.Subscriptions, liveLocalSub(ProviderNMI, fmt.Sprintf("nmi-%d", i)))
	}
	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		Capabilities: Capabilities{Subscriptions: true},
		Coverage:     SnapshotCoverage{SubscriptionsExhaustive: true},
		Subscriptions: []RemoteSubscription{
			{RailSubscriptionID: "nmi-0", Status: SubscriptionStatusActive},
		},
	}
	eng, store, writer := newTestEngine(ProviderNMI, snap, local)
	res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "circuit breaker")
	assert.Equal(t, "failed", res.Status)
	assert.True(t, res.Summary.Providers["nmi"].Aborted)
	assert.Zero(t, store.count(), "no findings may be persisted on a breaker abort")
	assert.Zero(t, writer.totalCalls())
}

// #837: the breaker used to be DISABLED below ten local live subscriptions —
// this exact fixture asserted that five subscribers could all be cancelled off
// an empty roster. Small books need MORE protection, not an exemption, so the
// floor is gone and a nine-subscriber merchant is protected too.
func TestSmallMerchantsAreProtectedFromEmptyRosters(t *testing.T) {
	ctx := context.Background()
	for _, book := range []int{1, 5, 9} {
		local := &fakeLocal{}
		for i := 0; i < book; i++ {
			local.state.Subscriptions = append(local.state.Subscriptions, liveLocalSub(ProviderNMI, fmt.Sprintf("nmi-%d", i)))
		}
		// An exhaustive-but-empty roster: the shape a misdeclared account_id or
		// a rotated credential produces.
		snap := &RemoteSnapshot{
			Provider:     ProviderNMI,
			Capabilities: Capabilities{Subscriptions: true},
			Coverage:     SnapshotCoverage{SubscriptionsExhaustive: true},
		}
		eng, _, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
		require.Error(t, err, "book of %d: an empty roster must not sail through", book)
		assert.Contains(t, err.Error(), "circuit breaker")
		assert.True(t, res.Summary.Providers["nmi"].Aborted)
		assert.Empty(t, findByType(res.Findings, FindingLocalActiveRemoteDead), "book of %d", book)
		assert.Zero(t, writer.calls["cancel"], "book of %d", book)
	}
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
			{RailSubscriptionID: "sub_stable", Status: SubscriptionStatusCancelled},
		},
	}
	eng, store, _ := newTestEngine(ProviderStripe, snap, local)

	res1, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderStripe}, PSPs: testPSPs(ProviderStripe)})
	require.NoError(t, err)
	require.Len(t, res1.Findings, 1)
	assert.Equal(t, 1, res1.Summary.Providers["stripe"].NewFindings)

	res2, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderStripe}, PSPs: testPSPs(ProviderStripe)})
	require.NoError(t, err)
	require.Len(t, res2.Findings, 1)
	assert.Equal(t, 0, res2.Summary.Providers["stripe"].NewFindings)
	assert.Equal(t, 1, res2.Summary.Providers["stripe"].UpdatedFindings)

	assert.Equal(t, 1, store.count(), "re-runs must update the standing finding, not duplicate it")
	rec := store.record(ProviderStripe, FindingLocalActiveRemoteDead, sub.ID.String())
	assert.Equal(t, &res1.RunID, rec.FirstSeenRun)
	assert.Equal(t, &res2.RunID, rec.LastSeenRun)
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
			{RailSubscriptionID: "sub_vanish", Status: SubscriptionStatusCancelled},
		},
	}
	eng, store, _ := newTestEngine(ProviderStripe, deadSnap, local)
	_, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderStripe}, PSPs: testPSPs(ProviderStripe)})
	require.NoError(t, err)
	require.Equal(t, FindingStatusReconcileRequired, store.record(ProviderStripe, FindingLocalActiveRemoteDead, sub.ID.String()).Status)

	// The drift disappears (rail reactivated / first read was wrong).
	eng.Fetchers[ProviderStripe] = &fakeFetcher{provider: ProviderStripe, snap: &RemoteSnapshot{
		Provider:     ProviderStripe,
		Capabilities: Capabilities{Subscriptions: true},
		Subscriptions: []RemoteSubscription{
			{RailSubscriptionID: "sub_vanish", Status: SubscriptionStatusActive, NextBillingAt: tp(*sub.CurrentPeriodEndsAt)},
		},
	}}
	res2, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderStripe}, PSPs: testPSPs(ProviderStripe)})
	require.NoError(t, err)

	rec := store.record(ProviderStripe, FindingLocalActiveRemoteDead, sub.ID.String())
	assert.Equal(t, FindingStatusFixed, rec.Status)
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
			{RailSubscriptionID: "nmi-intent", Status: SubscriptionStatusActive},
		},
	}
	eng, _, writer := newTestEngine(ProviderNMI, snap, local)
	res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
	require.NoError(t, err)

	ps3 := findByType(res.Findings, FindingStatusMismatch)
	require.Len(t, ps3, 1)
	assert.Equal(t, FindingStatusReconcileRequired, ps3[0].Status, "intent-annotated drift must not escalate to the admin queue")
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
	pm := LocalPaymentMethod{ID: uuid.New(), CustomerID: sub.CustomerID, Rail: "nmi", RailCustomerRef: "vault-gate", LastFour: "1111", ExpiryDate: "1025"}
	local.state.Subscriptions = []LocalSubscription{sub}
	local.state.PaymentMethods = []LocalPaymentMethod{pm}
	subID := sub.ID
	local.payments = []LocalPayment{
		{ID: uuid.New(), CustomerID: sub.CustomerID, Rail: "nmi", TransactionID: "txn-gate", AmountCents: 999, Status: "completed", SubscriptionID: &subID, PurchasedAt: testNow.Add(-time.Hour)},
	}
	// Snapshot contains a chargeback and a divergent vault entry, but the
	// provider declares neither capability: both checks must be skipped.
	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		Capabilities: Capabilities{Subscriptions: true, Transactions: true, Refunds: false, Chargebacks: false, Vault: false},
		Subscriptions: []RemoteSubscription{
			{RailSubscriptionID: "nmi-gate", Status: SubscriptionStatusActive, NextBillingAt: tp(*sub.CurrentPeriodEndsAt)},
		},
		Transactions: []RemoteTransaction{
			{TransactionID: "cb-1", Type: TransactionTypeChargeback, Success: true, AmountCents: 999, OccurredAt: testNow.Add(-time.Hour), Raw: rawJSON(map[string]any{"order_id": sub.ID.String()})},
			{TransactionID: "rf-1", Type: TransactionTypeRefund, Success: true, AmountCents: 999, OccurredAt: testNow.Add(-time.Hour), Raw: rawJSON(map[string]any{"order_id": sub.ID.String()})},
		},
		PaymentMethods: []RemotePaymentMethod{{RailCustomerRef: "vault-gate", CardLast4: "9999", CardExpiry: "1299"}},
	}
	eng, _, _ := newTestEngine(ProviderNMI, snap, local)
	res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
	require.NoError(t, err)

	assert.Empty(t, findByType(res.Findings, FindingChargebackActiveSub), "PS-6 must be capability-gated")
	assert.Empty(t, findByType(res.Findings, FindingRefundUnrecorded), "PS-5 must be capability-gated")
	assert.Empty(t, findByType(res.Findings, FindingPaymentMethodMismatch), "PS-7 must be capability-gated")
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
			{RailSubscriptionID: "nmi-idem", Status: SubscriptionStatusExpired, RawStatus: "expired"},
		},
	}
	eng, store, writer := newTestEngine(ProviderNMI, snap, local)

	res1, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
	require.NoError(t, err)
	assert.Equal(t, 1, res1.Summary.Providers["nmi"].AutoFixed)
	firstCalls := writer.totalCalls()
	assert.Positive(t, firstCalls)

	// Second enforce run: local state already converged, so the diff is empty
	// and no write happens.
	res2, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
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
			{RailSubscriptionID: "nmi-adv", Status: SubscriptionStatusCancelled},
		},
	}
	eng, store, writer := newTestEngine(ProviderNMI, snap, local)
	res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
	require.NoError(t, err)
	require.NotEmpty(t, res.Findings)
	assert.Zero(t, writer.totalCalls(), "advisory mode performs zero local writes")
	rec := store.record(ProviderNMI, FindingLocalActiveRemoteDead, dead.ID.String())
	assert.Equal(t, FindingStatusReconcileRequired, rec.Status)
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
			{RailSubscriptionID: "sub_dismiss", Status: SubscriptionStatusCancelled},
		},
	}
	eng, store, writer := newTestEngine(ProviderStripe, snap, local)
	_, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderStripe}, PSPs: testPSPs(ProviderStripe)})
	require.NoError(t, err)

	rec := store.record(ProviderStripe, FindingLocalActiveRemoteDead, sub.ID.String())
	store.mu.Lock()
	rec.Status = FindingStatusIgnored
	store.mu.Unlock()

	_, err = eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderStripe}, PSPs: testPSPs(ProviderStripe)})
	require.NoError(t, err)
	rec = store.record(ProviderStripe, FindingLocalActiveRemoteDead, sub.ID.String())
	assert.Equal(t, FindingStatusIgnored, rec.Status)
	assert.Zero(t, writer.totalCalls(), "ignored findings are never applied")
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
		Currency:  "USD",
		PSPLinks: map[string]map[string]string{
			// The provider-link key is the merchant's ACCOUNT key with the
			// rail recorded inside the entry.
			"mobius": {"rail": "nmi", "plan_id": "plan-gold"},
		},
	}
	local.state.Prices = []LocalPrice{price}
	local.state.PaymentMethods = []LocalPaymentMethod{{
		ID: uuid.New(), CustomerID: subjectID, Rail: "nmi",
		RailCustomerRef: "vault-77", LastFour: "1111", ExpiryDate: "1029",
	}}
	end := testNow.Add(20 * 24 * time.Hour)
	lastBilled := testNow.Add(-10 * 24 * time.Hour)
	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		Capabilities: Capabilities{Subscriptions: true, Transactions: true, Vault: true},
		Subscriptions: []RemoteSubscription{
			{
				RailSubscriptionID: "remote-77",
				Status:             SubscriptionStatusActive,
				CustomerID:         "vault-77",
				Email:              "owner@example.com",
				PlanID:             "plan-gold",
				NextBillingAt:      &end,
				LastBilledAt:       &lastBilled,
				AmountCents:        999,
				Currency:           "USD",
			},
		},
		Transactions: []RemoteTransaction{
			{
				TransactionID: "txn-mat-1", Type: TransactionTypeSale, Success: true,
				AmountCents: 999, Currency: "USD", OccurredAt: lastBilled,
				Raw: rawJSON(map[string]any{"customer_vault_id": "vault-77"}),
			},
		},
		PaymentMethods: []RemotePaymentMethod{
			{RailCustomerRef: "vault-77", CardLast4: "1111", CardExpiry: "1029"},
		},
	}
	return local, snap, price
}

func TestMaterializePS1(t *testing.T) {
	ctx := context.Background()

	t.Run("advisory PS-1 stays requires_review (advisory never writes)", func(t *testing.T) {
		local, snap, _ := materializeFixture()
		eng, store, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
		require.NoError(t, err)
		ps1 := findByType(res.Findings, FindingRemoteSubMissingLocal)
		require.Len(t, ps1, 1)
		assert.Equal(t, FindingStatusAdminRequired, ps1[0].Status)
		assert.True(t, ps1[0].RequiresAdmin)
		assert.Zero(t, writer.calls["materialize"])
		assert.Equal(t, FindingStatusAdminRequired, store.record(ProviderNMI, FindingRemoteSubMissingLocal, "remote-77").Status)
	})

	t.Run("resolvable PS-1 materializes with payment + entitlements and resolves enforced", func(t *testing.T) {
		local, snap, price := materializeFixture()
		eng, store, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
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
		st, _ := local.Load(ctx, ProviderNMI, uuid.Nil)
		var created *LocalSubscription
		for i := range st.Subscriptions {
			if st.Subscriptions[i].RailSubscriptionID == "remote-77" {
				created = &st.Subscriptions[i]
			}
		}
		require.NotNil(t, created)
		assert.Equal(t, "active", created.Status)
		assert.Equal(t, "nmi", created.Rail)
		require.NotNil(t, created.CurrentPeriodEndsAt)
		assert.True(t, created.CurrentPeriodEndsAt.Equal(*snap.Subscriptions[0].NextBillingAt))

		// …the snapshot charge is backfilled and entitlements granted.
		payments, _ := local.PaymentsByTransactionIDs(ctx, ProviderNMI, uuid.Nil, []string{"txn-mat-1"})
		require.Len(t, payments, 1)
		ents := local.entsSnapshot()
		require.Len(t, ents, 1)
		assert.Equal(t, created.ID, ents[0].SourceID)

		// Re-run: converged, no duplicate, no second materialize write.
		res2, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
		require.NoError(t, err)
		assert.Empty(t, findByType(res2.Findings, FindingRemoteSubMissingLocal))
		assert.Equal(t, 1, writer.calls["materialize"])
	})

	t.Run("ambiguous identity stays requires_review with the blocker documented", func(t *testing.T) {
		local, snap, _ := materializeFixture()
		// Second subject shares the remote email -> two distinct candidates.
		other := liveLocalSub(ProviderNMI, "other-1")
		other.UserEmail = "owner@example.com"
		local.state.Subscriptions = append(local.state.Subscriptions, other)
		snap.Subscriptions = append(snap.Subscriptions, RemoteSubscription{
			RailSubscriptionID: "other-1", Status: SubscriptionStatusActive,
		})
		eng, _, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
		require.NoError(t, err)
		ps1 := findByType(res.Findings, FindingRemoteSubMissingLocal)
		require.Len(t, ps1, 1)
		assert.Equal(t, FindingStatusAdminRequired, ps1[0].Status)
		assert.Contains(t, ps1[0].RemoteEvidence["materialize_blocked"], "ambiguous")
		assert.Zero(t, writer.calls["materialize"])
	})

	t.Run("unresolvable plan stays requires_review", func(t *testing.T) {
		local, snap, _ := materializeFixture()
		local.state.Prices = nil // no provider link anywhere
		eng, _, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
		require.NoError(t, err)
		ps1 := findByType(res.Findings, FindingRemoteSubMissingLocal)
		require.Len(t, ps1, 1)
		assert.Equal(t, FindingStatusAdminRequired, ps1[0].Status)
		assert.Contains(t, ps1[0].RemoteEvidence["materialize_blocked"], "plan unresolved")
		assert.Zero(t, writer.calls["materialize"])
	})

	t.Run("remote past_due without a period end stays requires_review", func(t *testing.T) {
		local, snap, _ := materializeFixture()
		snap.Subscriptions[0].Status = SubscriptionStatusPastDue
		snap.Subscriptions[0].NextBillingAt = nil
		eng, _, writer := newTestEngine(ProviderNMI, snap, local)
		res, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
		require.NoError(t, err)
		ps1 := findByType(res.Findings, FindingRemoteSubMissingLocal)
		require.Len(t, ps1, 1)
		assert.Equal(t, FindingStatusAdminRequired, ps1[0].Status)
		assert.Contains(t, ps1[0].RemoteEvidence["materialize_blocked"], "past_due")
		assert.Zero(t, writer.calls["materialize"])
	})
}

// --- forensics: Postgres third evidence source ---------------------------------

type fakeHistorySource struct {
	configured bool
	events     []HistoryEvent
	err        error
}

func (f *fakeHistorySource) Configured() bool { return f.configured }
func (f *fakeHistorySource) ListEvents(ctx context.Context, rails []string, since, until time.Time) ([]HistoryEvent, error) {
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
				{RailSubscriptionID: "nmi-hist", Status: SubscriptionStatusPastDue, NextBillingAt: tp(*sub.CurrentPeriodEndsAt)},
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
			{Table: "payment_events", EventType: "charge_failed", Rail: "nmi", SubscriptionID: &subID, OccurredAt: histAt},
			{Table: "payment_events", EventType: "charge_success", Rail: "nmi", SubscriptionID: &subID, OccurredAt: histAt.Add(-30 * 24 * time.Hour)},
			{Table: "subscription_events", EventType: "subscription_cancelled", Rail: "nmi", RailSubscriptionID: "nmi-hist", OccurredAt: histAt.Add(24 * time.Hour)},
			{Table: "payment_events", EventType: "charge_failed", Rail: "nmi", OccurredAt: histAt}, // uncorrelated: no ids
		}}
		res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
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

	t.Run("unreachable history source degrades to a note, never an error", func(t *testing.T) {
		local, snap, _ := newFixture()
		eng, _, _ := newTestEngine(ProviderNMI, snap, local)
		eng.History = &fakeHistorySource{configured: true, err: fmt.Errorf("dial tcp: connection refused")}
		res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
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
		res, err := eng.Run(ctx, RunParams{Mode: ModeAdvisory, Providers: []Provider{ProviderNMI}, PSPs: testPSPs(ProviderNMI)})
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

// testPSPs is the or#893 binding every pull now requires: a pass is bound to
// the ONE PSP whose credentials armed its fetcher. Unit fakes ignore the id;
// what matters is that the engine refuses to run without it.
func testPSPs(providers ...Provider) map[Provider]RailMerchantAccountBinding {
	out := make(map[Provider]RailMerchantAccountBinding, len(providers))
	for _, p := range providers {
		out[p] = RailMerchantAccountBinding{ID: uuid.New(), Rail: string(p), AccountID: string(p) + "-test-account"}
	}
	return out
}

// or#893: a pull section with no PSP binding used to run account-agnostically —
// it read the rail's ENTIRE local mirror (so PSP A's roster judged PSP B's
// subscriptions) and stamped every row it wrote with NULL provenance. The
// binding is now a precondition, and the refusal is the whole point: the pull
// plane always knows which PSP armed it, so an unbound section is a wiring bug,
// never a lane to fall back to.
func TestRunRefusesAProviderSectionWithNoPSPBinding(t *testing.T) {
	ctx := context.Background()
	local := &fakeLocal{}
	dead := liveLocalSub(ProviderNMI, "vanished-1")
	local.state.Subscriptions = []LocalSubscription{dead}
	withLiveEntitlement(local, &dead)
	snap := &RemoteSnapshot{
		Provider:      ProviderNMI,
		Capabilities:  Capabilities{Subscriptions: true},
		Coverage:      SnapshotCoverage{SubscriptionsExhaustive: true},
		Subscriptions: []RemoteSubscription{},
	}
	eng, _, writer := newTestEngine(ProviderNMI, snap, local)

	_, err := eng.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no PSP binding for provider nmi")
	// And it refused BEFORE touching local state: no cancel, no mirror write.
	assert.Zero(t, writer.totalCalls())

	// A zero-uuid binding is the same absence wearing a struct.
	_, err = eng.Run(ctx, RunParams{
		Mode:      ModeEnforce,
		Providers: []Provider{ProviderNMI},
		PSPs:      map[Provider]RailMerchantAccountBinding{ProviderNMI: {Rail: "nmi", AccountID: "mobius"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no PSP binding for provider nmi")
	assert.Zero(t, writer.totalCalls())
}

// or#893: every local write the pass plans carries the pull's PSP. Before, a
// nil binding short-circuited bindApplyActions and the mirror row landed with
// NULL provenance — invisible to a PSP-scoped prune, and indistinguishable from
// the row a sibling PSP would have produced.
func TestApplyActionsCarryThePullsPSP(t *testing.T) {
	psp := uuid.New()
	findings := []Finding{
		{Apply: &ApplyAction{BackfillPayment: &BackfillPaymentAction{TransactionID: "txn-1"}}},
		{Apply: &ApplyAction{RecordRefund: &RecordRefundAction{TransactionID: "re-1"}}},
		{Apply: &ApplyAction{Materialize: &MaterializeSubscriptionAction{
			RailSubscriptionID: "sub-1",
			Backfill:           &BackfillPaymentAction{TransactionID: "txn-2"},
		}}},
	}
	bindApplyActions(findings, psp)
	require.NotNil(t, findings[0].Apply.BackfillPayment.PspID)
	assert.Equal(t, psp, *findings[0].Apply.BackfillPayment.PspID)
	require.NotNil(t, findings[1].Apply.RecordRefund.PspID)
	assert.Equal(t, psp, *findings[1].Apply.RecordRefund.PspID)
	require.NotNil(t, findings[2].Apply.Materialize.PspID)
	assert.Equal(t, psp, *findings[2].Apply.Materialize.PspID)
	require.NotNil(t, findings[2].Apply.Materialize.Backfill.PspID)
	assert.Equal(t, psp, *findings[2].Apply.Materialize.Backfill.PspID)
}
