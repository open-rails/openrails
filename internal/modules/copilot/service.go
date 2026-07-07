// Package copilot implements #779: the console catalog copilot. It extends
// the #741/#756 LLM surface (schema-carries-the-intelligence, haiku-first,
// fail-closed feature gates) from METRICS to the CATALOG: merchants ask
// questions about products/prices/subscribers and, once #781 lands, draft
// price changes conversationally.
//
// The one safety principle (non-negotiable, per the 2026-07-07 design
// ruling): the model NEVER mutates. Its only write-shaped output is a DRAFT
// of the same structured payload the human console produces (a #777 wizard
// plan or a catalog diff), rendered through the wizard's own review step for
// human confirmation. Every constraint the API enforces (same-currency,
// same-product, active-price) is enforced there, not reimplemented here —
// LLM proposes, primitives dispose.
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

// Deps are the catalog copilot's collaborators. LLM may be nil (the feature
// fails closed); Enabled is the llm.catalog_copilot_enabled consent (Q&A
// sends aggregate catalog/subscriber-count data to the provider, like #756);
// DraftingEnabled additionally arms the Phase 2 draft_* tools — MUST stay
// false until #781 ships (see doctrine.go); Limiter may be nil (no
// rate limiting).
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

// DraftingConfigured reports whether Phase 2 drafting tools are armed. Gated
// on the SAME deployment flag as #781's presence per the tracker ruling:
// stays false — drafting tools absent from the tool list entirely, not
// present-but-erroring — until an operator flips llm.catalog_drafting_enabled
// (intended: after both #779 and #781 merge).
func (s *Service) DraftingConfigured() bool { return s.Configured() && s.drafting }

func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}
