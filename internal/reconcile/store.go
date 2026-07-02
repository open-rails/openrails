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
	FirstSeenRun      uuid.UUID      `json:"first_seen_run"`
	LastSeenRun       uuid.UUID      `json:"last_seen_run"`
	LastSeenAt        time.Time      `json:"last_seen_at"`
	ResolvedAt        *time.Time     `json:"resolved_at,omitempty"`
	Resolution        string         `json:"resolution,omitempty"`
	Notes             string         `json:"operator_notes,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
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
		Providers:   names,
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
		RunID:             runID,
	})
	if err != nil {
		return FindingRecord{}, err
	}
	return findingRecordFromRow(row), nil
}

func (s *PGStore) ListActionableFindingsByProvider(ctx context.Context, provider Provider) ([]FindingRecord, error) {
	providerStr := string(provider)
	rows, err := s.DB.Gen(ctx).ListActionableReconciliationFindingsByProvider(ctx, &providerStr)
	if err != nil {
		return nil, err
	}
	out := make([]FindingRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, findingRecordFromRow(row))
	}
	return out, nil
}

func (s *PGStore) AutoResolveVanished(ctx context.Context, provider Provider, runID uuid.UUID, types []FindingType) (int64, error) {
	names := make([]string, 0, len(types))
	for _, t := range types {
		names = append(names, string(t))
	}
	providerStr := string(provider)
	return s.DB.Gen(ctx).AutoResolveVanishedReconciliationFindings(ctx, gen.AutoResolveVanishedReconciliationFindingsParams{
		Provider:     &providerStr,
		RunID:        runID,
		FindingTypes: names,
	})
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
	return findingRecordFromRow(row), nil
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
		out = append(out, findingRecordFromRow(row))
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

func findingRecordFromRow(row gen.OpenrailsReconciliationFinding) FindingRecord {
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
	}
	if row.RecommendedAction != nil {
		rec.RecommendedAction = *row.RecommendedAction
	}
	if row.Resolution != nil {
		rec.Resolution = *row.Resolution
	}
	if row.OperatorNotes != nil {
		rec.Notes = *row.OperatorNotes
	}
	return rec
}

func runRecordFromRow(row gen.OpenrailsReconciliationRun) RunRecord {
	rec := RunRecord{
		ID:          row.ID,
		MerchantID:  row.MerchantID,
		Mode:        Mode(row.Mode),
		Providers:   row.Providers,
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
