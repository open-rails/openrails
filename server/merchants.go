package server

import (
	"context"
	"errors"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchants"
)

// The operator's merchant directory, and a merchant's name and API host.
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
// credentials resolve nothing, while every record stays. Deleting a deleted
// merchant changes nothing.
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
// under Config.Auth.Naming. With req.ActorUserID, the user renaming their
// own merchant, the name also answers to Config.MerchantCreation's reserved
// names and pattern and to the rename interval (*billing.MerchantRenameTooSoonError).
func (s *Server) RenameMerchant(ctx context.Context, id billing.MerchantID, req billing.RenameMerchantParams) (*billing.MerchantName, error) {
	m, err := s.cp.RenameMerchant(ctx, id, req.Name, req.ActorUserID, req.ActorUserID == "")
	if err != nil {
		return nil, err
	}
	return &billing.MerchantName{ID: m.ID, Name: m.Slug}, nil
}

// ClaimMerchantAPIHost is a merchant's own claim on host for its public
// routes: the host routes nothing until VerifyMerchantAPIHost finds the
// claim's token in DNS. "" releases the host and any claim, the current host
// changes nothing, and the deployment's own hosts are ErrAPIHostReserved.
// The operator binds a host without proof with SetMerchantAPIHost.
func (s *Server) ClaimMerchantAPIHost(ctx context.Context, id billing.MerchantID, host string) (*billing.MerchantAPIHost, error) {
	rt := s.graph.Runtime
	current, claim, err := rt.Merchants.ChangeAPIHost(ctx, id, host, rt.ReservedAPIHosts)
	if err != nil {
		return nil, err
	}
	view := merchants.APIHostView(current, claim)
	return &view, nil
}

// VerifyMerchantAPIHost proves the merchant's open claim through DNS and
// binds its host. Until the record carries the token it is
// ErrAPIHostUnproven, with the claim (the record to publish) in the result.
func (s *Server) VerifyMerchantAPIHost(ctx context.Context, id billing.MerchantID) (*billing.MerchantAPIHost, error) {
	rt := s.graph.Runtime
	host, err := rt.Merchants.VerifyAPIHost(ctx, id, rt.ReservedAPIHosts, rt.DNSResolver)
	if errors.Is(err, merchants.ErrAPIHostUnproven) {
		cfg, cerr := rt.Merchants.GetHostConfig(ctx, id)
		claim, clerr := rt.Merchants.APIHostClaimOf(ctx, id)
		if cerr != nil || clerr != nil {
			return nil, errors.Join(err, cerr, clerr)
		}
		view := merchants.APIHostView(cfg.APIHost, claim)
		return &view, err
	}
	if err != nil {
		return nil, err
	}
	view := merchants.APIHostView(host, nil)
	return &view, nil
}
