package reconcile

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// FindingRecord is a persisted finding (one stable identity row).
type FindingRecord struct {
	ID                uuid.UUID      `json:"id"`
	MerchantID        uuid.UUID      `json:"tenant_id"`
	Provider          Provider       `json:"provider,omitempty"`
	Type              FindingType    `json:"finding_type"`
	SubjectKey        string         `json:"subject_key"`
	Severity          Severity       `json:"severity"`
	Status            FindingStatus  `json:"status"`
	RequiresReview    bool           `json:"requires_review,omitempty"`
	RequiresAdmin     bool           `json:"-"`
	RecommendedAction string         `json:"recommended_action,omitempty"`
	Evidence          map[string]any `json:"evidence,omitempty"`
	LocalEvidence     map[string]any `json:"-"`
	RemoteEvidence    map[string]any `json:"-"`
	IntentEvidence    map[string]any `json:"-"`
	ResolutionEvid    map[string]any `json:"-"`
	FirstSeenRun      *uuid.UUID     `json:"first_seen_run,omitempty"`
	LastSeenRun       *uuid.UUID     `json:"last_seen_run,omitempty"`
	LastSeenAt        time.Time      `json:"last_seen_at"`
	ResolvedAt        *time.Time     `json:"resolved_at,omitempty"`
	Resolution        string         `json:"resolution,omitempty"`
	ResolvedBy        string         `json:"resolved_by,omitempty"`
	Notes             string         `json:"operator_notes,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
	// NotifiedAt/NotifiedSeverity are the #787 operator-notification dedupe
	// linkage: NotifiedAt nil means this open finding has not yet pushed a
	// notification; NotifiedSeverity is the severity it was notified at, so a
	// FindingNotifier can detect a genuine escalation vs mere re-observation.
	// Cleared on every resolution (see the reconciliation.sql resolve queries).
	NotifiedAt       *time.Time `json:"notified_at,omitempty"`
	NotifiedSeverity string     `json:"notified_severity,omitempty"`
}

// RunRecord is a persisted reconciliation run.
type RunRecord struct {
	ID          uuid.UUID       `json:"id"`
	MerchantID  uuid.UUID       `json:"tenant_id"`
	Mode        Mode            `json:"mode"`
	Providers   []string        `json:"providers"`
	WindowSince *time.Time      `json:"window_since,omitempty"`
	WindowUntil *time.Time      `json:"window_until,omitempty"`
	StartedAt   time.Time       `json:"started_at"`
	FinishedAt  *time.Time      `json:"finished_at,omitempty"`
	Status      string          `json:"status"`
	Summary     json.RawMessage `json:"summary,omitempty"`
	Error       string          `json:"error,omitempty"`
}

// Store is the engine-facing persistence surface. The pg implementation runs
// merchant-scoped; the unit-test fake is in-memory.
type Store interface {
	CreateRun(ctx context.Context, mode Mode, providers []Provider, since, until *time.Time) (uuid.UUID, error)
	FinishRun(ctx context.Context, runID uuid.UUID, status string, summary []byte, runErr string) error
	UpsertFinding(ctx context.Context, runID uuid.UUID, f Finding) (FindingRecord, error)
	ListActionableFindingsByProvider(ctx context.Context, provider Provider) ([]FindingRecord, error)
	AutoResolveVanished(ctx context.Context, provider Provider, runID uuid.UUID, types []FindingType) (int64, error)
	MarkFindingVanished(ctx context.Context, id uuid.UUID) error
	MarkFindingAutoFixed(ctx context.Context, id uuid.UUID, resolutionEvidence map[string]any) error
	// MarkFindingNotified stamps the #787 dedupe linkage after a FindingNotifier
	// successfully pushes an operator notification for an OPEN finding.
	MarkFindingNotified(ctx context.Context, id uuid.UUID, at time.Time, severity Severity) error
}

// PGStore persists runs + findings via the sqlc layer on a merchant-pinned
// connection.
type PGStore struct {
	DB *db.DB
}

var _ Store = (*PGStore)(nil)

func marshalEvidence(m map[string]any) []byte {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"evidence_encode_error": err.Error()})
	}
	return b
}

func unmarshalEvidence(b []byte) map[string]any {
	if len(b) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{"evidence_decode_error": err.Error()}
	}
	return m
}

func findingEvidence(f Finding) []byte {
	evidence := map[string]any{}
	if f.Provider != "" && f.Provider != "self" {
		evidence["provider"] = string(f.Provider)
	}
	if len(f.LocalEvidence) > 0 {
		evidence["local"] = f.LocalEvidence
	}
	if len(f.RemoteEvidence) > 0 {
		evidence["remote"] = f.RemoteEvidence
	}
	if len(f.IntentEvidence) > 0 {
		evidence["intent"] = f.IntentEvidence
	}
	return marshalEvidence(evidence)
}

func nestedEvidence(evidence map[string]any, key string) map[string]any {
	if len(evidence) == 0 {
		return nil
	}
	nested, _ := evidence[key].(map[string]any)
	return nested
}

func (s *PGStore) CreateRun(ctx context.Context, mode Mode, providers []Provider, since, until *time.Time) (uuid.UUID, error) {
	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, string(p))
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	row, err := s.DB.Gen(ctx).CreateReconciliationRun(ctx, gen.CreateReconciliationRunParams{
		MerchantID:  tid.UUID(),
		Mode:        string(mode),
		Rails:       names,
		WindowSince: since,
		WindowUntil: until,
	})
	if err != nil {
		return uuid.Nil, err
	}
	return row.ID, nil
}

func (s *PGStore) FinishRun(ctx context.Context, runID uuid.UUID, status string, summary []byte, runErr string) error {
	var errPtr *string
	if runErr != "" {
		errPtr = &runErr
	}
	_, err := s.DB.Gen(ctx).FinishReconciliationRun(ctx, gen.FinishReconciliationRunParams{
		ID:      runID,
		Status:  status,
		Summary: summary,
		Error:   errPtr,
	})
	return err
}

func (s *PGStore) UpsertFinding(ctx context.Context, runID uuid.UUID, f Finding) (FindingRecord, error) {
	var action *string
	if f.RecommendedAction != "" {
		action = &f.RecommendedAction
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return FindingRecord{}, err
	}
	row, err := s.DB.Gen(ctx).UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID:        tid.UUID(),
		FindingType:       string(f.Type),
		SubjectKey:        f.SubjectKey,
		Severity:          string(f.Severity),
		Status:            string(f.Status),
		RecommendedAction: action,
		Evidence:          findingEvidence(f),
		RunID:             &runID,
	})
	if err != nil {
		return FindingRecord{}, err
	}
	return FindingRecordFromRow(row), nil
}

func (s *PGStore) ListActionableFindingsByProvider(ctx context.Context, provider Provider) ([]FindingRecord, error) {
	providerStr := string(provider)
	rows, err := s.DB.Gen(ctx).ListActionableReconciliationFindingsByProvider(ctx, &providerStr)
	if err != nil {
		return nil, err
	}
	out := make([]FindingRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, FindingRecordFromRow(row))
	}
	return out, nil
}

// autoResolveBatch bounds one auto-resolve statement (or#837). The whole
// backlog still resolves — the loop below runs until a short batch — but as
// many short transactions instead of one that holds the findings table for the
// length of the backlog.
const autoResolveBatch = 1000

func (s *PGStore) AutoResolveVanished(ctx context.Context, provider Provider, runID uuid.UUID, types []FindingType) (int64, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	names := make([]string, 0, len(types))
	for _, t := range types {
		names = append(names, string(t))
	}
	providerStr := string(provider)
	var total int64
	for {
		n, err := s.DB.Gen(ctx).AutoResolveVanishedReconciliationFindings(ctx, gen.AutoResolveVanishedReconciliationFindingsParams{
			MerchantID:   tid.UUID(),
			Provider:     &providerStr,
			RunID:        &runID,
			FindingTypes: names,
			RowLimit:     autoResolveBatch,
		})
		total += n
		if err != nil {
			return total, err
		}
		if n < autoResolveBatch {
			return total, nil
		}
	}
}

func (s *PGStore) MarkFindingVanished(ctx context.Context, id uuid.UUID) error {
	_, err := s.DB.Gen(ctx).MarkReconciliationFindingVanished(ctx, id)
	return err
}

func (s *PGStore) MarkFindingAutoFixed(ctx context.Context, id uuid.UUID, resolutionEvidence map[string]any) error {
	_, err := s.DB.Gen(ctx).MarkReconciliationFindingAutoFixed(ctx, gen.MarkReconciliationFindingAutoFixedParams{
		ID:                 id,
		ResolutionEvidence: marshalEvidence(resolutionEvidence),
	})
	return err
}

func (s *PGStore) MarkFindingNotified(ctx context.Context, id uuid.UUID, at time.Time, severity Severity) error {
	_, err := s.DB.Gen(ctx).MarkReconciliationFindingNotified(ctx, gen.MarkReconciliationFindingNotifiedParams{
		ID: id, NotifiedAt: at, Severity: string(severity),
	})
	return err
}

// --- admin/report reads + lifecycle (used by the CLI and admin API, not the
// engine interface) ---

// FindingFilter narrows ListFindings.
type FindingFilter struct {
	Status         string
	Provider       string
	Type           string
	OnlyAdminQueue bool
	Limit          int
	Offset         int
}

func (s *PGStore) GetRun(ctx context.Context, id uuid.UUID) (RunRecord, error) {
	row, err := s.DB.Gen(ctx).GetReconciliationRun(ctx, id)
	if err != nil {
		return RunRecord{}, err
	}
	return runRecordFromRow(row), nil
}

func (s *PGStore) GetLatestRun(ctx context.Context) (RunRecord, error) {
	row, err := s.DB.Gen(ctx).GetLatestReconciliationRun(ctx)
	if err != nil {
		return RunRecord{}, err
	}
	return runRecordFromRow(row), nil
}

func (s *PGStore) ListRuns(ctx context.Context, limit, offset int) ([]RunRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.DB.Gen(ctx).ListReconciliationRuns(ctx, gen.ListReconciliationRunsParams{
		PageLimit:  int64(limit),
		PageOffset: int64(offset),
	})
	if err != nil {
		return nil, err
	}
	out := make([]RunRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, runRecordFromRow(row))
	}
	return out, nil
}

func (s *PGStore) GetFinding(ctx context.Context, id uuid.UUID) (FindingRecord, error) {
	row, err := s.DB.Gen(ctx).GetReconciliationFinding(ctx, id)
	if err != nil {
		return FindingRecord{}, err
	}
	return FindingRecordFromRow(row), nil
}

func (s *PGStore) ListFindings(ctx context.Context, filter FindingFilter) ([]FindingRecord, error) {
	if filter.Limit <= 0 {
		filter.Limit = 100
	}
	params := gen.ListReconciliationFindingsParams{
		OnlyReviewQueue: filter.OnlyAdminQueue,
		PageLimit:       int64(filter.Limit),
		PageOffset:      int64(filter.Offset),
	}
	if filter.Status != "" {
		params.Status = &filter.Status
	}
	if filter.Provider != "" {
		params.Provider = &filter.Provider
	}
	if filter.Type != "" {
		params.FindingType = &filter.Type
	}
	rows, err := s.DB.Gen(ctx).ListReconciliationFindings(ctx, params)
	if err != nil {
		return nil, err
	}
	out := make([]FindingRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, FindingRecordFromRow(row))
	}
	return out, nil
}

// AckFinding resolves a finding as admin-acknowledged. Returns false when the
// finding was not in an ackable state.
func (s *PGStore) AckFinding(ctx context.Context, id uuid.UUID, notes string) (bool, error) {
	var notesPtr *string
	if notes != "" {
		notesPtr = &notes
	}
	n, err := s.DB.Gen(ctx).AckReconciliationFinding(ctx, gen.AckReconciliationFindingParams{ID: id, OperatorNotes: notesPtr})
	return n > 0, err
}

// DismissFinding marks a finding ignored; ignored identities stay
// ignored across re-runs.
func (s *PGStore) DismissFinding(ctx context.Context, id uuid.UUID, notes string) (bool, error) {
	var notesPtr *string
	if notes != "" {
		notesPtr = &notes
	}
	n, err := s.DB.Gen(ctx).DismissReconciliationFinding(ctx, gen.DismissReconciliationFindingParams{ID: id, OperatorNotes: notesPtr})
	return n > 0, err
}

// --- #692 operator findings queue (admin API) ---

// #690 gauge type sets: named counts over OPEN findings, one per error
// category (Orphaned / Freeloader / Double-Billed). The detectors live in the
// converge DERIVE/CON passes; extend the sets here when a new type joins a
// gauge, not the queries.
var (
	// OrphanedFindingTypes: PAYING WITHOUT ACCESS — the MISSING side (money
	// collected, entitlement absent/wrongly revoked). Only the ADMIN-side
	// type counts: derive.subscription.missing and derive.wallet.missing are
	// AUTO-repaired in the same sweep and never sit open (same rationale that
	// keeps the dead-subs AUTO check out of freeloaders) — they are episode
	// material (openrails.orphaned_episodes), not standing errors.
	// derive.grant.missing is ADMIN surface-only and DOES sit open.
	OrphanedFindingTypes = []string{
		"derive.grant.missing",
	}
	// FreeloaderFindingTypes: ACCESS WITHOUT PAYING — live access whose
	// source is PROVEN dead or absent (#691: stale ≠ freeloader). The
	// dead-sub-live-window check (derive.grant_effect.mismatch, revoke
	// direction) is deliberately NOT in the set: it is AUTO-repaired (the
	// missed #691 closure) in the same sweep, so it never sits open — its
	// standing-window shape surfaces here as derive.entitlement.unjustified
	// instead.
	FreeloaderFindingTypes = []string{
		"derive.entitlement.unjustified",
		"derive.grant_effect.excess",
	}
	// DuplicateCoverageFindingTypes: DOUBLE-BILLED — the same (customer,
	// product) holding overlapping paid coverage twice.
	DuplicateCoverageFindingTypes = []string{
		"consistency.duplicate.ownership",
		"consistency.duplicate.provider_charge",
		string(FindingDuplicateSubscriptions),
	}
)

// QueueFilter narrows the operator work list. Empty Status = open findings
// only (reconcile_required | requires_review).
type QueueFilter struct {
	Severity string
	Type     string
	Status   string
	Limit    int
	Offset   int
}

// ListQueueFindings returns the operator work list: severity desc (critical
// first) then age desc (oldest first), paginated, with the unpaginated total.
func (s *PGStore) ListQueueFindings(ctx context.Context, filter QueueFilter) ([]FindingRecord, int64, error) {
	if filter.Limit <= 0 || filter.Limit > 500 {
		filter.Limit = 100
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, 0, err
	}
	params := gen.AdminListReconciliationFindingsParams{
		MerchantID: tid.UUID(),
		PageLimit:  int64(filter.Limit),
		PageOffset: int64(filter.Offset),
	}
	if filter.Status != "" {
		params.Status = &filter.Status
	}
	if filter.Severity != "" {
		params.Severity = &filter.Severity
	}
	if filter.Type != "" {
		params.FindingType = &filter.Type
	}
	rows, err := s.DB.Gen(ctx).AdminListReconciliationFindings(ctx, params)
	if err != nil {
		return nil, 0, err
	}
	out := make([]FindingRecord, 0, len(rows))
	var total int64
	for _, row := range rows {
		total = row.TotalCount
		out = append(out, FindingRecordFromRow(row.OpenrailsReconciliationFinding))
	}
	return out, total, nil
}

// QueueGauges is the #690 dashboard header. OrphanedMembers, Freeloaders and
// DuplicateCoverage are ALWAYS-ZERO error metrics (counts over OPEN findings
// of the three category type sets); VerificationPressure is a live pressure
// reading, allowed to be nonzero; Episodes is the historical/interval measure
// (spans + error-days from the migration-067 views, approximations documented
// there).
type QueueGauges struct {
	OrphanedMembers      int64                `json:"orphaned_members"`
	Freeloaders          int64                `json:"freeloaders"`
	DuplicateCoverage    int64                `json:"duplicate_coverage"`
	VerificationPressure VerificationPressure `json:"verification_pressure"`
	Episodes             EpisodeTotals        `json:"episodes"`
	OpenBySeverity       map[string]int64     `json:"open_by_severity"`
	TotalOpen            int64                `json:"total_open"`
}

// EpisodeTotals (#690): compact summary of the two episode views — the
// historical/interval companions to the point-in-time gauges. A freeloader/
// orphaned episode is a SPAN (access-without-payment / payment-without-access)
// with open episodes still accruing at now().
type EpisodeTotals struct {
	Freeloader FreeloaderEpisodeSummary `json:"freeloader"`
	Orphaned   EpisodeSummary           `json:"orphaned"`
}

// EpisodeSummary: episode count, how many are still open (accruing), and the
// total error-days across all spans.
type EpisodeSummary struct {
	Total     int64   `json:"total"`
	Open      int64   `json:"open"`
	TotalDays float64 `json:"total_days"`
}

// FreeloaderEpisodeSummary adds the `unsanctioned` split: sanctioned_dunning
// and awaiting_verification spans are POLICY (deliberate unpaid access), never
// failure — only unsanctioned spans indicate the state machine failed.
type FreeloaderEpisodeSummary struct {
	EpisodeSummary
	Unsanctioned int64 `json:"unsanctioned"`
}

// VerificationPressure (#690/#691): subscriptions parked `unknown` whose
// recorded paid-through has passed — fail-open standing access awaiting
// provider verification. Computed live from the subscriptions table, NOT from
// findings: it measures drift, not error. MaxAgeSeconds is the age of the
// oldest lapsed paid-through; the COUNT may legitimately be nonzero, but the
// max age trending UP means the verification machinery (pull/probe/converge)
// is down.
type VerificationPressure struct {
	Count         int64 `json:"count"`
	MaxAgeSeconds int64 `json:"max_age_seconds"`
}

// Gauges folds open-finding counts into the named gauges and reads the live
// verification pressure.
func (s *PGStore) Gauges(ctx context.Context) (QueueGauges, error) {
	g := QueueGauges{OpenBySeverity: map[string]int64{}}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return g, err
	}
	rows, err := s.DB.Gen(ctx).CountOpenReconciliationFindingsByTypeSeverity(ctx, tid.UUID())
	if err != nil {
		return g, err
	}
	inSet := func(set []string, t string) bool {
		for _, s := range set {
			if s == t {
				return true
			}
		}
		return false
	}
	for _, row := range rows {
		g.TotalOpen += row.OpenCount
		g.OpenBySeverity[row.Severity] += row.OpenCount
		if inSet(OrphanedFindingTypes, row.FindingType) {
			g.OrphanedMembers += row.OpenCount
		}
		if inSet(FreeloaderFindingTypes, row.FindingType) {
			g.Freeloaders += row.OpenCount
		}
		if inSet(DuplicateCoverageFindingTypes, row.FindingType) {
			g.DuplicateCoverage += row.OpenCount
		}
	}
	pressure, err := s.DB.Gen(ctx).CountUnknownSubsPastPaidThrough(ctx, gen.CountUnknownSubsPastPaidThroughParams{
		MerchantID: tid.UUID(), Now: time.Now().UTC(),
	})
	if err != nil {
		return g, err
	}
	g.VerificationPressure.Count = pressure.PressureCount
	if pressure.MaxAgeSeconds != nil {
		g.VerificationPressure.MaxAgeSeconds = *pressure.MaxAgeSeconds
	}
	episodes, err := s.DB.Gen(ctx).CountErrorEpisodeTotals(ctx, tid.UUID())
	if err != nil {
		return g, err
	}
	g.Episodes = EpisodeTotals{
		Freeloader: FreeloaderEpisodeSummary{
			EpisodeSummary: EpisodeSummary{
				Total:     episodes.FreeloaderTotal,
				Open:      episodes.FreeloaderOpen,
				TotalDays: episodes.FreeloaderDays,
			},
			Unsanctioned: episodes.FreeloaderUnsanctioned,
		},
		Orphaned: EpisodeSummary{
			Total:     episodes.OrphanedTotal,
			Open:      episodes.OrphanedOpen,
			TotalDays: episodes.OrphanedDays,
		},
	}
	return g, nil
}

// ResolveFindingFixed marks an OPEN finding fixed/admin_fixed with operator
// notes, attribution and execution evidence (evidence.resolution). Returns
// false when the finding was not open.
func (s *PGStore) ResolveFindingFixed(ctx context.Context, id uuid.UUID, notes, resolvedBy string, resolutionEvidence map[string]any) (bool, error) {
	var notesPtr *string
	if notes != "" {
		notesPtr = &notes
	}
	n, err := s.DB.Gen(ctx).AdminResolveReconciliationFinding(ctx, gen.AdminResolveReconciliationFindingParams{
		ID:                 id,
		OperatorNotes:      notesPtr,
		ResolvedBy:         &resolvedBy,
		ResolutionEvidence: marshalEvidence(resolutionEvidence),
	})
	return n > 0, err
}

// IgnoreFindingWithActor marks an OPEN finding ignored with attribution —
// permanent silence for the subject (upserts keep ignored identities ignored).
func (s *PGStore) IgnoreFindingWithActor(ctx context.Context, id uuid.UUID, notes, resolvedBy string) (bool, error) {
	var notesPtr *string
	if notes != "" {
		notesPtr = &notes
	}
	n, err := s.DB.Gen(ctx).AdminIgnoreReconciliationFinding(ctx, gen.AdminIgnoreReconciliationFindingParams{
		ID:            id,
		OperatorNotes: notesPtr,
		ResolvedBy:    &resolvedBy,
	})
	return n > 0, err
}

// AppendFindingNotes appends an execution-failure note to an OPEN finding
// (partial failure: the finding stays open, never half-marked fixed).
func (s *PGStore) AppendFindingNotes(ctx context.Context, id uuid.UUID, note string) error {
	_, err := s.DB.Gen(ctx).AppendReconciliationFindingNotes(ctx, gen.AppendReconciliationFindingNotesParams{
		ID:   id,
		Note: note,
	})
	return err
}

func FindingRecordFromRow(row gen.OpenrailsReconciliationFinding) FindingRecord {
	evidence := unmarshalEvidence(row.Evidence)
	provider, _ := evidence["provider"].(string)
	requiresReview := row.Status == string(FindingStatusRequiresReview)
	rec := FindingRecord{
		ID:             row.ID,
		MerchantID:     row.MerchantID,
		Provider:       Provider(provider),
		Type:           FindingType(row.FindingType),
		SubjectKey:     row.SubjectKey,
		Severity:       Severity(row.Severity),
		Status:         FindingStatus(row.Status),
		RequiresReview: requiresReview,
		RequiresAdmin:  requiresReview,
		Evidence:       evidence,
		LocalEvidence:  nestedEvidence(evidence, "local"),
		RemoteEvidence: nestedEvidence(evidence, "remote"),
		IntentEvidence: nestedEvidence(evidence, "intent"),
		ResolutionEvid: nestedEvidence(evidence, "resolution"),
		FirstSeenRun:   row.FirstSeenRun,
		LastSeenRun:    row.LastSeenRun,
		LastSeenAt:     row.LastSeenAt,
		ResolvedAt:     row.ResolvedAt,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
		NotifiedAt:     row.NotifiedAt,
	}
	if row.RecommendedAction != nil {
		rec.RecommendedAction = *row.RecommendedAction
	}
	if row.Resolution != nil {
		rec.Resolution = *row.Resolution
	}
	if row.ResolvedBy != nil {
		rec.ResolvedBy = *row.ResolvedBy
	}
	if row.OperatorNotes != nil {
		rec.Notes = *row.OperatorNotes
	}
	if row.NotifiedSeverity != nil {
		rec.NotifiedSeverity = *row.NotifiedSeverity
	}
	return rec
}

func runRecordFromRow(row gen.OpenrailsReconciliationRun) RunRecord {
	rec := RunRecord{
		ID:          row.ID,
		MerchantID:  row.MerchantID,
		Mode:        Mode(row.Mode),
		Providers:   row.Rails,
		WindowSince: row.WindowSince,
		WindowUntil: row.WindowUntil,
		StartedAt:   row.StartedAt,
		FinishedAt:  row.FinishedAt,
		Status:      row.Status,
		Summary:     row.Summary,
	}
	if row.Error != nil {
		rec.Error = *row.Error
	}
	return rec
}
