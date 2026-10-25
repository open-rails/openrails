package server

import (
	"context"

	"github.com/open-rails/openrails/billing"
)

// The operator's merchant directory and a merchant's name.
// OpenRails serves none of these over HTTP: the operator calls them (or the
// openrails CLI), and a hosted product builds its own routes on them. The
// caller authorizes and audits each call.

// ListMerchants pages the merchant directory, newest first: active merchants
// unless params name statuses.
func (s *Server) ListMerchants(ctx context.Context, params billing.MerchantListParams) (*billing.ListPage[billing.Merchant], error) {
	return s.graph.Runtime.Merchants.ListDirectory(ctx, params)
}

// GetMerchant is one merchant in any status; ErrMerchantNotFound when none.
func (s *Server) GetMerchant(ctx context.Context, id billing.MerchantID) (*billing.Merchant, error) {
	return s.graph.Runtime.Merchants.DirectoryEntry(ctx, id)
}

// DeleteMerchant soft-deletes a merchant: it leaves the default lists and its
// credentials resolve nothing. Every billing record stays. Deleting an already
// deleted merchant changes nothing. Hosts own their external relationship lifecycle.
func (s *Server) DeleteMerchant(ctx context.Context, id billing.MerchantID) (*billing.Merchant, error) {
	return s.graph.Runtime.Merchants.SoftDelete(ctx, id)
}

// RestoreMerchant undoes DeleteMerchant; restoring an active merchant changes
// nothing. A retired merchant is billing.ErrConflict, and a name another
// merchant has taken since billing.ErrMerchantNameTaken.
func (s *Server) RestoreMerchant(ctx context.Context, id billing.MerchantID) (*billing.Merchant, error) {
	return s.graph.Runtime.Merchants.Restore(ctx, id)
}

// RenameMerchant renames an active merchant; its former name forwards to it
// under Config.Naming. With req.ActorUserID, the user renaming their
// own merchant, the name also answers to Config.MerchantCreation's reserved
// names and pattern and to the rename interval (*billing.MerchantRenameTooSoonError).
func (s *Server) RenameMerchant(ctx context.Context, id billing.MerchantID, req billing.RenameMerchantParams) (*billing.MerchantName, error) {
	m, err := s.cp.RenameMerchant(ctx, id, req.Name, req.ActorUserID, req.ActorUserID == "")
	if err != nil {
		return nil, err
	}
	return &billing.MerchantName{ID: m.ID, Name: m.Slug}, nil
}
