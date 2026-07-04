package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Service persists per-merchant dashboards and drives NL widget generation.
// All reads/writes ride the request's RLS-pinned merchant connection.
type Service struct {
	db    *db.DB
	llm   LLM
	clock clockwork.Clock
}

// NewService binds the dashboard module. llm may be nil (NL generation
// disabled, fail-closed); clock nil = wall clock.
func NewService(database *db.DB, llm LLM, clock clockwork.Clock) *Service {
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	return &Service{db: database, llm: llm, clock: clock}
}

// SetLLM swaps the generation backend (tests inject a deterministic stub).
func (s *Service) SetLLM(l LLM) { s.llm = l }

// NLConfigured reports whether natural-language generation is available.
func (s *Service) NLConfigured() bool { return s != nil && s.llm != nil }

// Get returns the merchant's saved dashboard, or the seeded default template
// when none exists (usage widgets only if the merchant has usage activity).
func (s *Service) Get(ctx context.Context) (*Dashboard, error) {
	q := s.db.Gen(ctx)
	row, err := q.GetDashboardConfig(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		hasUsage, err := q.HasUsageActivity(ctx)
		if err != nil {
			return nil, fmt.Errorf("dashboard: usage-activity probe: %w", err)
		}
		return &Dashboard{Widgets: DefaultWidgets(hasUsage), IsDefault: true}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dashboard: load config: %w", err)
	}
	var widgets []Widget
	if err := json.Unmarshal(row.Layout, &widgets); err != nil {
		return nil, fmt.Errorf("dashboard: corrupt stored layout: %w", err)
	}
	return &Dashboard{Widgets: widgets, UpdatedAt: &row.UpdatedAt, UpdatedBy: row.UpdatedBy}, nil
}

// Put validates and replaces the merchant's dashboard. Every widget query must
// pass the metrics compiler; errors return widget-indexed and all-at-once.
func (s *Service) Put(ctx context.Context, widgets []Widget, updatedBy string) (*Dashboard, error) {
	if verr := ValidateWidgets(widgets); verr != nil {
		return nil, verr
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, fmt.Errorf("dashboard: no merchant in context: %w", err)
	}
	layout, err := json.Marshal(widgets)
	if err != nil {
		return nil, fmt.Errorf("dashboard: marshal layout: %w", err)
	}
	var by *string
	if updatedBy != "" {
		by = &updatedBy
	}
	row, err := s.db.Gen(ctx).UpsertDashboardConfig(ctx, gen.UpsertDashboardConfigParams{
		MerchantID: merchantID.UUID(),
		Layout:     layout,
		UpdatedBy:  by,
	})
	if err != nil {
		return nil, fmt.Errorf("dashboard: save config: %w", err)
	}
	return &Dashboard{Widgets: widgets, UpdatedAt: &row.UpdatedAt, UpdatedBy: row.UpdatedBy}, nil
}
