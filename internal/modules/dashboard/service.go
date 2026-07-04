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
	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/pkg/merchant"
)

// MetricsExecutor executes validated metrics plans (the #733 service; an
// interface so ask-loop unit tests inject canned results).
type MetricsExecutor interface {
	Execute(ctx context.Context, plan *metrics.Plan) (*metrics.Result, error)
}

// Service persists per-merchant dashboards, drives NL widget generation and
// the #756 metrics Q&A loop. All reads/writes ride the request's RLS-pinned
// merchant connection.
type Service struct {
	db         *db.DB
	metrics    MetricsExecutor
	llm        LLM
	askEnabled bool
	askLimiter AskLimiter
	clock      clockwork.Clock
}

// Deps are the dashboard module's collaborators. LLM may be nil (NL features
// fail closed); AskEnabled is the llm.ask_enabled consent flag; AskLimiter may
// be nil (no per-merchant ask rate limiting); Clock nil = wall clock.
type Deps struct {
	DB         *db.DB
	Metrics    MetricsExecutor
	LLM        LLM
	AskEnabled bool
	AskLimiter AskLimiter
	Clock      clockwork.Clock
}

// NewService binds the dashboard module.
func NewService(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = clockwork.NewRealClock()
	}
	return &Service{
		db:         d.DB,
		metrics:    d.Metrics,
		llm:        d.LLM,
		askEnabled: d.AskEnabled,
		askLimiter: d.AskLimiter,
		clock:      d.Clock,
	}
}

// SetLLM swaps the model backend (tests inject a deterministic stub).
func (s *Service) SetLLM(l LLM) { s.llm = l }

// NLConfigured reports whether natural-language generation is available.
func (s *Service) NLConfigured() bool { return s != nil && s.llm != nil }

// AskConfigured reports whether metrics Q&A is available: an LLM AND the
// explicit llm.ask_enabled consent (query results flow to the provider).
func (s *Service) AskConfigured() bool { return s.NLConfigured() && s.askEnabled }

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
