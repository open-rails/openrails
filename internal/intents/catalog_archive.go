package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// Catalog archive intent types (#358 phase D): the provider writes of
// `push-catalog --prune` (#357) flow through the ledger. Archives are
// idempotent verify-then-execute (the remote object is read first;
// already-archived/absent = success with evidence) and admin-origin (the
// --prune flag is an explicit human request), so they execute under
// mode=limited and park under readonly — a provider being down no longer
// aborts the sweep, those intents retry durably.
//
// NMI deliberately has NO archive intent type: NMI has no plan-archive
// primitive and plan edits/deletes affect live subscribers (#357 verdict), so
// NMI extras stay manual-action-only and no NMI write path may be added here.
const (
	TypeStripeArchiveProduct = "stripe_archive_product"
	TypeStripeArchivePrice   = "stripe_archive_price"
)

// StripeArchiveIdempotencyKey content-addresses one logical archive: the
// intent type (provider + operation) plus the remote object id.
func StripeArchiveIdempotencyKey(intentType, externalID string) string {
	return intentType + ":" + strings.TrimSpace(externalID)
}

// StripeArchivePayload is the stored payload for both Stripe archive types.
// MarkerKey is the OpenRails ownership marker captured at detection time (a
// product slug for products, a price content key for prices); relevance
// re-checks it against the live local catalog.
type StripeArchivePayload struct {
	ObjectID  string `json:"object_id"`
	MarkerKey string `json:"marker_key"`
	Label     string `json:"label,omitempty"`
}

func decodeStripeArchivePayload(intent gen.OpenrailsProviderIntent) (StripeArchivePayload, error) {
	var p StripeArchivePayload
	if len(intent.Payload) == 0 {
		return p, errors.New("stripe archive intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, fmt.Errorf("decode stripe archive payload: %w", err)
	}
	if strings.TrimSpace(p.ObjectID) == "" {
		return p, errors.New("stripe archive payload is incomplete (object_id is required)")
	}
	return p, nil
}

// stripeCatalogAPI is the StripeCatalogService slice the archive handlers
// drive (interface for unit tests). Find* answer absence as a first-class
// result so verify-then-execute can treat 404 as success.
type stripeCatalogAPI interface {
	FindProduct(ctx context.Context, stripeProductID string) (*catalog.StripeProduct, bool, error)
	FindPrice(ctx context.Context, stripePriceID string) (*catalog.StripePrice, bool, error)
	UpdateProduct(ctx context.Context, stripeProductID string, params catalog.UpdateProductParams) error
	UpdatePrice(ctx context.Context, stripePriceID string, params catalog.UpdatePriceParams) error
}

// catalogRowsLoader loads the full local catalog for the relevance re-check.
// A field (not a method) so unit tests can stub it without a database.
type catalogRowsLoader func(ctx context.Context) ([]*models.Product, []*models.Price, error)

func dbCatalogRowsLoader(d *db.DB) catalogRowsLoader {
	return func(ctx context.Context) ([]*models.Product, []*models.Price, error) {
		products, err := catalog.NewProductService(d).GetAll(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("load products: %w", err)
		}
		prices, err := catalog.NewPriceService(d).GetAll(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("load prices: %w", err)
		}
		return products, prices, nil
	}
}

// stripeArchiveCore is the shared machinery of the two Stripe archive
// handlers; the per-type bits (object kind, read, write) are parameterized.
type stripeArchiveCore struct {
	Config      *config.Config
	Rails       config.RailSet
	Stripe      stripeCatalogAPI
	LoadCatalog catalogRowsLoader
	Policy      BackoffPolicy
}

func (h *stripeArchiveCore) Backoff(attempts int32) time.Duration { return h.Policy.Delay(attempts) }

func (h *stripeArchiveCore) stripeConfigured() bool {
	if h.Rails == nil {
		// nil config = unit-test harness; the injected API decides.
		return h.Stripe != nil
	}
	proc := h.Rails.GetStripeRail()
	return proc != nil && strings.TrimSpace(proc.SecretKey) != "" && h.Stripe != nil
}

// checkRelevance: the archive applies while the remote object is STILL an
// extra — not linked by id and not content-matched by its ownership marker. If
// the object has since been added/linked locally, archiving the remote copy
// would be wrong -> superseded. Local reads only; the same extra-ness
// definition detection uses (catalog.ExtrasIndex).
func (h *stripeArchiveCore) checkRelevance(ctx context.Context, intent gen.OpenrailsProviderIntent, isExtra func(catalog.ExtrasIndex, StripeArchivePayload) bool) (Relevance, error) {
	p, err := decodeStripeArchivePayload(intent)
	if err != nil {
		// Malformed payloads can never become executable; superseding surfaces
		// them in reconcile instead of re-parking forever.
		return SupersededBy("unusable stripe archive intent: " + err.Error()), nil
	}
	products, prices, err := h.LoadCatalog(ctx)
	if err != nil {
		return Relevance{}, err
	}
	if !isExtra(catalog.BuildExtrasIndex(products, prices), p) {
		return SupersededBy("remote object is now in the local catalog; archiving it would be wrong"), nil
	}
	return StillRelevant(), nil
}

// execute is the shared verify-then-execute: read the remote object first —
// absent or already inactive is success with evidence; only a live active
// object is archived (active=false, with the intent's idempotency_key as the
// Stripe Idempotency-Key). Archives are idempotent, so a failed write is
// cleanly retryable — never ambiguous.
func (h *stripeArchiveCore) execute(
	ctx context.Context,
	intent gen.OpenrailsProviderIntent,
	read func(ctx context.Context, id string) (active bool, found bool, err error),
	write func(ctx context.Context, id, idempotencyKey string) error,
) Outcome {
	if !h.stripeConfigured() {
		return Parked("stripe not configured")
	}
	p, err := decodeStripeArchivePayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	active, found, err := read(ctx, p.ObjectID)
	if err != nil {
		return Retryable("provider read before archive failed: " + err.Error())
	}
	if !found {
		return Succeeded(map[string]any{"verified_absent": true, "object_id": p.ObjectID})
	}
	if !active {
		return Succeeded(map[string]any{"already_inactive": true, "object_id": p.ObjectID})
	}
	if err := write(ctx, p.ObjectID, intent.IdempotencyKey); err != nil {
		if errors.Is(err, stripeapi.ErrProviderReadOnly) {
			return Parked("stripe provider writes blocked (mode=readonly)")
		}
		return Retryable("stripe archive failed: " + err.Error())
	}
	return Succeeded(map[string]any{"archived": true, "object_id": p.ObjectID})
}

// verify resolves any in-doubt archive with reads only: inactive/absent at the
// provider means done; still active means it definitely has not happened.
func (h *stripeArchiveCore) verify(
	ctx context.Context,
	intent gen.OpenrailsProviderIntent,
	read func(ctx context.Context, id string) (active bool, found bool, err error),
) Outcome {
	if !h.stripeConfigured() {
		return Ambiguous("stripe not configured; cannot verify")
	}
	p, err := decodeStripeArchivePayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	active, found, err := read(ctx, p.ObjectID)
	if err != nil {
		return Ambiguous("provider read failed: " + err.Error())
	}
	if !found {
		return Succeeded(map[string]any{"verified_absent": true, "object_id": p.ObjectID})
	}
	if !active {
		return Succeeded(map[string]any{"verified_inactive": true, "object_id": p.ObjectID})
	}
	return Retryable("object still active at provider; archive verified not executed")
}

// ============================================================================
// stripe_archive_product
// ============================================================================

// StripeArchiveProductHandler archives a Stripe Product (active=false).
type StripeArchiveProductHandler struct {
	stripeArchiveCore
}

func NewStripeArchiveProductHandler(d *db.DB, cfg *config.Config, rails config.RailSet, _ clockwork.Clock) *StripeArchiveProductHandler {
	return &StripeArchiveProductHandler{stripeArchiveCore{
		Config:      cfg,
		Rails:       rails,
		Stripe:      &catalog.StripeCatalogService{Config: cfg, Rails: rails},
		LoadCatalog: dbCatalogRowsLoader(d),
		Policy:      DefaultBackoff,
	}}
}

func (h *StripeArchiveProductHandler) Type() string { return TypeStripeArchiveProduct }

func (h *StripeArchiveProductHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsProviderIntent) (Relevance, error) {
	return h.checkRelevance(ctx, intent, func(ix catalog.ExtrasIndex, p StripeArchivePayload) bool {
		extra, _ := ix.StripeProductExtra(catalog.StripeProduct{
			ID:       p.ObjectID,
			Metadata: map[string]string{catalog.StripeMetadataOpenRailsProductKey: p.MarkerKey},
		})
		return extra
	})
}

func (h *StripeArchiveProductHandler) readProduct(ctx context.Context, id string) (bool, bool, error) {
	obj, found, err := h.Stripe.FindProduct(ctx, id)
	if err != nil || !found {
		return false, found, err
	}
	return obj.Active, true, nil
}

func (h *StripeArchiveProductHandler) Execute(ctx context.Context, intent gen.OpenrailsProviderIntent) Outcome {
	return h.execute(ctx, intent, h.readProduct, func(ctx context.Context, id, key string) error {
		inactive := false
		return h.Stripe.UpdateProduct(ctx, id, catalog.UpdateProductParams{Active: &inactive, IdempotencyKey: key})
	})
}

func (h *StripeArchiveProductHandler) Verify(ctx context.Context, intent gen.OpenrailsProviderIntent) Outcome {
	return h.verify(ctx, intent, h.readProduct)
}

// ============================================================================
// stripe_archive_price
// ============================================================================

// StripeArchivePriceHandler archives a Stripe Price (active=false).
type StripeArchivePriceHandler struct {
	stripeArchiveCore
}

func NewStripeArchivePriceHandler(d *db.DB, cfg *config.Config, rails config.RailSet, _ clockwork.Clock) *StripeArchivePriceHandler {
	return &StripeArchivePriceHandler{stripeArchiveCore{
		Config:      cfg,
		Rails:       rails,
		Stripe:      &catalog.StripeCatalogService{Config: cfg, Rails: rails},
		LoadCatalog: dbCatalogRowsLoader(d),
		Policy:      DefaultBackoff,
	}}
}

func (h *StripeArchivePriceHandler) Type() string { return TypeStripeArchivePrice }

func (h *StripeArchivePriceHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsProviderIntent) (Relevance, error) {
	return h.checkRelevance(ctx, intent, func(ix catalog.ExtrasIndex, p StripeArchivePayload) bool {
		extra, _ := ix.StripePriceExtra(catalog.StripePrice{
			ID:       p.ObjectID,
			Metadata: map[string]string{catalog.StripeMetadataOpenRailsPriceKey: p.MarkerKey},
		})
		return extra
	})
}

func (h *StripeArchivePriceHandler) readPrice(ctx context.Context, id string) (bool, bool, error) {
	obj, found, err := h.Stripe.FindPrice(ctx, id)
	if err != nil || !found {
		return false, found, err
	}
	return obj.Active, true, nil
}

func (h *StripeArchivePriceHandler) Execute(ctx context.Context, intent gen.OpenrailsProviderIntent) Outcome {
	return h.execute(ctx, intent, h.readPrice, func(ctx context.Context, id, key string) error {
		inactive := false
		return h.Stripe.UpdatePrice(ctx, id, catalog.UpdatePriceParams{Active: &inactive, IdempotencyKey: key})
	})
}

func (h *StripeArchivePriceHandler) Verify(ctx context.Context, intent gen.OpenrailsProviderIntent) Outcome {
	return h.verify(ctx, intent, h.readPrice)
}
