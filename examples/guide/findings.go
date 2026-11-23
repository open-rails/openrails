package guide

import (
	"context"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// ApproveImport approves what one PSP import proposed, after review.
func ApproveImport(ctx context.Context, shop *openrails.Client, pull billing.PSPImportID, key string) error {
	// guide:proposals
	page, err := shop.ListProposals(ctx, billing.ProposalListParams{Pull: pull})
	if err != nil {
		return err
	}
	ids := make([]billing.FindingID, 0, len(page.Items))
	for _, p := range page.Items {
		ids = append(ids, p.ID) // each carries its Change: what applying it does
	}
	_, err = shop.ApproveProposals(ctx, billing.ApproveProposalsParams{IDs: ids, Notes: "reviewed"},
		openrails.WithIdempotencyKey(key))
	return err
	// guide:end
}
