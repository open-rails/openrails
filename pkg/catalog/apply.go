package catalog

import (
	"context"
	"fmt"
	"io"

	"github.com/google/uuid"

	billingservice "github.com/open-rails/openrails/pkg/service"
)

// ApplyResult summarizes what an Apply run did, including any per-provider
// manual actions surfaced by CreatePrice (e.g. CCBill, an unconfigured Solana
// plan) that the operator must complete out-of-band.
type ApplyResult struct {
	ProductsCreated  int                `json:"products_created"`
	ProductsUpdated  int                `json:"products_updated"`
	ProductsArchived int                `json:"products_archived"`
	PricesCreated    int                `json:"prices_created"`
	PricesUpdated    int                `json:"prices_updated"`
	PricesActivated  int                `json:"prices_activated"`
	PricesArchived   int                `json:"prices_archived"`
	PendingActions   []PendingActionFor `json:"pending_actions,omitempty"`
}

// ApplyOptions limits which mutation classes ApplyWithOptions executes. The
// zero value intentionally applies nothing; use Apply for the historical full
// convergence behavior.
type ApplyOptions struct {
	Insert    bool
	Overwrite bool
	Prune     bool
}

// PendingActionFor pairs a returned service.PendingAction with the price it
// belongs to, so the apply output can tell the operator which price needs a
// manual link.
type PendingActionFor struct {
	ProductKey string                       `json:"product_key"`
	PriceLabel string                       `json:"price_label"`
	Action     billingservice.PendingAction `json:"action"`
}

// Apply converges OpenRails onto the plan, driving the Applier. It is
// idempotent: re-running a converged plan is a no-op. Manual provider actions
// surfaced on price creation are collected into the result rather than failing.
func Apply(ctx context.Context, applier Applier, plan *ApplyPlan) (*ApplyResult, error) {
	return ApplyWithOptions(ctx, applier, plan, ApplyOptions{Insert: true, Overwrite: true, Prune: true})
}

// ApplyWithOptions converges only the requested mutation classes from a plan.
// It is used by the operator CLI's --insert/--overwrite/--prune contract.
func ApplyWithOptions(ctx context.Context, applier Applier, plan *ApplyPlan, opts ApplyOptions) (*ApplyResult, error) {
	res := &ApplyResult{}
	for gi := range plan.Groups {
		gp := &plan.Groups[gi]
		for pi := range gp.Products {
			pp := &gp.Products[pi]
			productID, err := applyProduct(ctx, applier, pp, res, opts)
			if err != nil {
				return res, err
			}
			if productID == uuid.Nil {
				continue
			}
			if err := applyPrices(ctx, applier, pp, productID, res, opts); err != nil {
				return res, err
			}
		}
		if !opts.Prune {
			continue
		}
		for i := range gp.RemovedProducts {
			rp := &gp.RemovedProducts[i]
			if _, err := applier.DeactivateProduct(ctx, rp.ID); err != nil {
				return res, fmt.Errorf("archive removed product %s: %w", rp.Key, err)
			}
			res.ProductsArchived++
		}
	}
	if plan != nil && plan.Manifest != nil && (opts.Insert || opts.Overwrite || opts.Prune) {
		if syncer, ok := applier.(interface {
			SyncCatalogSidecars(context.Context, *Manifest) error
		}); ok {
			if err := syncer.SyncCatalogSidecars(ctx, plan.Manifest); err != nil {
				return res, fmt.Errorf("sync catalog sidecars: %w", err)
			}
		}
	}
	return res, nil
}

func applyProduct(ctx context.Context, applier Applier, pp *ProductPlan, res *ApplyResult, opts ApplyOptions) (uuid.UUID, error) {
	switch pp.Action {
	case ProductCreate:
		if !opts.Insert {
			return uuid.Nil, nil
		}
		created, err := applier.CreateProduct(ctx, pp.CreateReq)
		if err != nil {
			return uuid.Nil, fmt.Errorf("create product %s: %w", pp.Key, err)
		}
		res.ProductsCreated++
		return created.ID, nil
	case ProductUpdate:
		if !opts.Overwrite {
			return pp.UpdateID, nil
		}
		updated, err := applier.UpdateProduct(ctx, pp.UpdateID, pp.UpdateReq)
		if err != nil {
			return uuid.Nil, fmt.Errorf("update product %s: %w", pp.Key, err)
		}
		res.ProductsUpdated++
		return updated.ID, nil
	default: // ProductUnchanged: nothing to push, but we still need the id for prices.
		return pp.UpdateID, nil
	}
}

func applyPrices(ctx context.Context, applier Applier, pp *ProductPlan, productID uuid.UUID, res *ApplyResult, opts ApplyOptions) error {
	for i := range pp.Prices {
		plp := &pp.Prices[i]
		switch plp.Action {
		case PriceCreate:
			if !opts.Insert {
				continue
			}
			req := plp.CreateReq
			req.ProductID = productID
			out, err := applier.CreatePrice(ctx, req)
			if err != nil {
				return fmt.Errorf("create price %s: %w", plp.Label, err)
			}
			res.PricesCreated++
			for _, pa := range out.PendingManualActions {
				res.PendingActions = append(res.PendingActions, PendingActionFor{
					ProductKey: pp.Key,
					PriceLabel: plp.Label,
					Action:     pa,
				})
			}
		case PriceActivate:
			if !opts.Overwrite {
				continue
			}
			if _, err := applier.ActivatePrice(ctx, plp.ExistingID); err != nil {
				return fmt.Errorf("activate price %s: %w", plp.Label, err)
			}
			res.PricesActivated++
		case PriceArchive:
			if !opts.Prune {
				continue
			}
			if _, err := applier.DeactivatePrice(ctx, plp.ExistingID); err != nil {
				return fmt.Errorf("archive price %s: %w", plp.Label, err)
			}
			res.PricesArchived++
		case PriceUpdate:
			// Provider link/config convergence runs below.
		case PriceUnchanged:
			// nothing to do beyond a possible key relabel below.
		}
		// PriceArchive is excluded on top of plan.go's own gate (belt and
		// braces): link sync must never publish provider objects for a price
		// this run is retiring.
		if plp.Action != PriceCreate && plp.Action != PriceArchive && len(plp.UpdateReq.PSPLinks) > 0 && opts.Overwrite {
			out, err := applier.UpdatePrice(ctx, plp.ExistingID, plp.UpdateReq)
			if err != nil {
				return fmt.Errorf("update price %s: %w", plp.Label, err)
			}
			// Count an update only when at least one requested provider link
			// actually applied. When every requested slot deferred to a pending
			// action nothing was stored, and reporting "updated" would claim
			// convergence an unconfigured deployment never made.
			if len(out.PendingManualActions) < len(plp.UpdateReq.PSPLinks) {
				res.PricesUpdated++
			}
			for _, pa := range out.PendingManualActions {
				res.PendingActions = append(res.PendingActions, PendingActionFor{
					ProductKey: pp.Key,
					PriceLabel: plp.Label,
					Action:     pa,
				})
			}
		}
		// #774: a substance-matched price declared under a renamed key (plp.Key
		// set only when it differs from the matched row's current key) gets
		// relabeled regardless of which match action fired above — a plain
		// label edit, gated on Overwrite like any other price mutation.
		if plp.Action != PriceCreate && plp.Key != "" && opts.Overwrite {
			if _, err := applier.SetPriceKey(ctx, plp.ExistingID, plp.Key); err != nil {
				return fmt.Errorf("relabel price %s to key %q: %w", plp.Label, plp.Key, err)
			}
		}
	}
	return nil
}

// Print writes a human summary of the apply result, including any pending
// manual actions, to out.
func (r *ApplyResult) Print(out io.Writer) {
	fmt.Fprintf(out, "applied: products +%d ~%d -%d | prices +%d updated %d activated %d archived %d\n",
		r.ProductsCreated, r.ProductsUpdated, r.ProductsArchived,
		r.PricesCreated, r.PricesUpdated, r.PricesActivated, r.PricesArchived)
	if len(r.PendingActions) == 0 {
		return
	}
	fmt.Fprintf(out, "\npending manual actions (%d) — these providers need an out-of-band step:\n", len(r.PendingActions))
	for i := range r.PendingActions {
		pa := &r.PendingActions[i]
		fmt.Fprintf(out, "  [%s] %s on price %s: %s\n",
			pa.Action.Provider, pa.Action.Action, pa.PriceLabel, pa.Action.Hint)
	}
}
