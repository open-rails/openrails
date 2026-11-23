package server

import (
	"context"
	"fmt"

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

// GetMerchant is one merchant in any status; billing.ErrMerchantNotFound when none.
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

// RenameMerchant is the operator's rename of an active merchant; its former
// name forwards to it under Config.Naming.
func (s *Server) RenameMerchant(ctx context.Context, id billing.MerchantID, req billing.RenameMerchantParams) (*billing.MerchantName, error) {
	return s.rename(ctx, id, req, "")
}

// RenameUserMerchant is userID's rename of their own merchant: the name also
// answers to Config.MerchantCreation's reserved names and pattern and to the
// rename interval (*billing.MerchantRenameTooSoonError).
func (s *Server) RenameUserMerchant(ctx context.Context, id billing.MerchantID, userID string, req billing.RenameMerchantParams) (*billing.MerchantName, error) {
	if userID == "" {
		return nil, fmt.Errorf("%w: a user rename names its user", billing.ErrInvalid)
	}
	return s.rename(ctx, id, req, userID)
}

func (s *Server) rename(ctx context.Context, id billing.MerchantID, req billing.RenameMerchantParams, userID string) (*billing.MerchantName, error) {
	m, err := s.cp.RenameMerchant(ctx, id, req.Name, userID, userID == "")
	if err != nil {
		return nil, err
	}
	return &billing.MerchantName{ID: m.ID, Name: m.Slug}, nil
}

// MerchantPurgeInventory records what purging a merchant would destroy and
// returns it. It copies nothing and restores nothing; PurgeMerchant requires it.
func (s *Server) MerchantPurgeInventory(ctx context.Context, id billing.MerchantID) (*billing.MerchantPurgeInventory, error) {
	inventory, err := s.graph.Runtime.Merchants.TakePurgeInventory(ctx, id)
	if err != nil {
		return nil, err
	}
	return &inventory, nil
}

// PurgeMerchant permanently deletes a merchant's rows and tombstones it, once
// params type back the inventory the operator read and the destructive-action
// policy allows it. A job queued with the purge deletes its Vault
// configuration, retried until none remains. Point-in-time recovery is the only
// way back.
func (s *Server) PurgeMerchant(ctx context.Context, id billing.MerchantID, params billing.PurgeMerchantParams) (*billing.MerchantPurge, error) {
	purge, err := s.graph.Runtime.Merchants.Purge(ctx, id, params)
	if err != nil {
		return nil, err
	}
	return &purge, nil
}
