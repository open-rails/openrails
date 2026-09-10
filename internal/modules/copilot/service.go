// Package copilot provides merchant-scoped catalog Q&A and optional drafting.
// The model can query catalog summaries but cannot mutate them. Draft tools
// return typed proposals for the console's human-reviewed mutation flow, where
// the API enforces authorization and catalog constraints again.
package copilot

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/query"
)

// ProductReader is the read-only product surface the copilot needs — the
// exact method set catalog.ProductService already exports (no adapter: the
// production wiring passes the real service directly).
type ProductReader interface {
	GetActive(ctx context.Context) ([]*models.Product, error)
	GetByKey(ctx context.Context, key string) (*models.Product, error)
	GetByID(ctx context.Context, id uuid.UUID) (*models.Product, error)
}

// PriceReader is the read-only price/key surface — catalog.PriceService's
// existing method set.
type PriceReader interface {
	GetActiveByProductID(ctx context.Context, productID uuid.UUID) ([]*models.Price, error)
	GetCurrentByKey(ctx context.Context, merchantID uuid.UUID, key string) (*models.Price, error)
	ListChainByKey(ctx context.Context, merchantID uuid.UUID, key string) ([]*models.Price, error)
	ListKeyMovements(ctx context.Context, merchantID uuid.UUID, key string) ([]*models.PriceKeyMovement, error)
	GetByID(ctx context.Context, id uuid.UUID) (*models.Price, error)
}

// SubscriptionCounter is the per-price subscriber-count primitive —
// subscriptions.SubscriptionService's existing GetSubscribers, called with
// Limit:0 so only the COUNT query runs (the items page is discarded).
type SubscriptionCounter interface {
	GetSubscribers(ctx context.Context, params query.QueryOptions[subscriptions.GetSubscriptionsFilters]) ([]*models.Subscription, int64, error)
}

// RepricePreviewer is the #773/#777 read-only reprice surface the copilot
// rides for affected-count previews and pending-migration lookups —
// subscriptions.RepriceService's existing method set. NEVER call a method
// beyond these three (Reprice / RepriceAllPriorVersions mutate; the copilot
// tool layer must never reach them).
type RepricePreviewer interface {
	PreviewAllPriorVersions(ctx context.Context, priceKey string) (*subscriptions.RepricePreviewResult, error)
	ListBatchesForKey(ctx context.Context, priceKey string, limit, offset int) ([]*models.RepriceBatch, error)
	List(ctx context.Context, filter subscriptions.SubscriptionRepriceFilter, limit, offset int) ([]*models.SubscriptionReprice, error)
}

// Deps are the catalog copilot's collaborators. LLM may be nil, which leaves
// the feature disabled. Enabled and Drafting are separate consent gates;
// Limiter may be nil when the caller accepts no rate limiting.
type Deps struct {
	Products ProductReader
	Prices   PriceReader
	Subs     SubscriptionCounter
	Reprices RepricePreviewer
	LLM      dashboard.LLM
	Enabled  bool
	Drafting bool
	Limiter  AskLimiter
	Clock    clockwork.Clock
}

// Service answers catalog Q&A (#779 Phase 1) and, when armed, drafts price
// changes / catalog diffs (Phase 2) for human review in the #777 wizard.
type Service struct {
	products ProductReader
	prices   PriceReader
	subs     SubscriptionCounter
	reprices RepricePreviewer
	llm      dashboard.LLM
	enabled  bool
	drafting bool
	limiter  AskLimiter
	clock    clockwork.Clock
}

func NewService(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = clockwork.NewRealClock()
	}
	return &Service{
		products: d.Products,
		prices:   d.Prices,
		subs:     d.Subs,
		reprices: d.Reprices,
		llm:      d.LLM,
		enabled:  d.Enabled,
		drafting: d.Drafting,
		limiter:  d.Limiter,
		clock:    d.Clock,
	}
}

// SetLLM swaps the model backend (tests inject a deterministic stub).
func (s *Service) SetLLM(l dashboard.LLM) { s.llm = l }

// Configured reports whether catalog Q&A can run: an LLM AND the explicit
// llm.catalog_copilot_enabled consent.
func (s *Service) Configured() bool { return s != nil && s.llm != nil && s.enabled }

// DraftingConfigured reports whether drafting tools may be offered to the
// model. Disabled tools are absent from the tool list.
func (s *Service) DraftingConfigured() bool { return s.Configured() && s.drafting }

func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}
