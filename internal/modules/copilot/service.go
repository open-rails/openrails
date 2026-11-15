// Package copilot answers merchant-scoped catalog questions and, when armed,
// drafts typed proposals for the console's human-reviewed mutation flow. The
// model never mutates the catalog.
package copilot

import (
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// Deps are the catalog copilot's collaborators. LLM may be nil, which leaves
// the feature disabled. Enabled and Drafting are separate consent gates;
// HTTP abuse limits belong to the shared route middleware.
type Deps struct {
	Products *catalog.ProductService
	Prices   *catalog.PriceService
	Subs     *subscriptions.SubscriptionService
	Reprices *subscriptions.PriceMigrationService
	LLM      LLM
	Enabled  bool
	Drafting bool
	Clock    clockwork.Clock
}

// Service answers catalog questions and, when armed, drafts price changes and
// new prices for human review in the console.
type Service struct {
	products *catalog.ProductService
	prices   *catalog.PriceService
	subs     *subscriptions.SubscriptionService
	reprices *subscriptions.PriceMigrationService
	llm      LLM
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
func (s *Service) SetLLM(l LLM) { s.llm = l }

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
