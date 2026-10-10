// Package copilot answers merchant-scoped catalog questions and, when armed,
// drafts typed proposals for the console's human-reviewed mutation flow. The
// model never mutates the catalog.
package copilot

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// ProductReader is the read-only product surface; catalog.ProductService
// satisfies it.
type ProductReader interface {
	GetActive(ctx context.Context) ([]*models.Product, error)
	GetByKey(ctx context.Context, key string) (*models.Product, error)
	GetByID(ctx context.Context, id uuid.UUID) (*models.Product, error)
}

// PriceReader is the read-only price surface; catalog.PriceService satisfies it.
type PriceReader interface {
	GetActiveByProductID(ctx context.Context, productID uuid.UUID) ([]*models.Price, error)
	GetCurrentByProductKey(ctx context.Context, merchantID uuid.UUID, productKey, key string) (*models.Price, error)
	ListChainByKey(ctx context.Context, merchantID, productID uuid.UUID, key string) ([]*models.Price, error)
	ListKeyMovements(ctx context.Context, merchantID, productID uuid.UUID, key string, page billing.PageRequest) (billing.ListPage[*models.PriceKeyMovement], error)
	GetByID(ctx context.Context, id uuid.UUID) (*models.Price, error)
}

// SubscriptionCounter is the per-price subscriber-count primitive.
type SubscriptionCounter interface {
	CountSubscribers(ctx context.Context, f subscriptions.GetSubscriptionsFilters) (int64, error)
}

// RepricePreviewer is the read-only price-migration surface the copilot rides
// for affected-count previews and pending-migration lookups.
type RepricePreviewer interface {
	Preview(ctx context.Context, params billing.CreatePriceMigrationParams) (*billing.PriceMigrationPreview, error)
	List(ctx context.Context, params billing.PriceMigrationListParams) (billing.ListPage[billing.PriceMigration], error)
}

// Deps are the catalog copilot's collaborators. LLM may be nil, which leaves
// the feature disabled. Enabled and Drafting are separate consent gates;
// HTTP abuse limits belong to the shared route middleware.
type Deps struct {
	Products ProductReader
	Prices   PriceReader
	Subs     SubscriptionCounter
	Reprices RepricePreviewer
	LLM      dashboard.LLM
	Enabled  bool
	Drafting bool
	Clock    clockwork.Clock
}

// Service answers catalog questions and, when armed, drafts price changes and
// new prices for human review in the console.
type Service struct {
	products ProductReader
	prices   PriceReader
	subs     SubscriptionCounter
	reprices RepricePreviewer
	llm      dashboard.LLM
	enabled  bool
	drafting bool
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
