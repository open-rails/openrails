package network

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/iam"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/customernetwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// linkPage bounds one read of an account's or a merchant's links.
const linkPage = 200

// Links are the account's active links, one per merchant.
func (s *Service) Links(ctx context.Context, account billing.NetworkAccountID) ([]billing.CustomerLink, error) {
	out := []billing.CustomerLink{}
	err := s.eachLink(ctx, account.UUID(), func(row gen.BillingCustomerLink) error {
		out = append(out, customernetwork.LinkOf(row))
		return nil
	})
	return out, err
}

// eachLink calls fn for each of the account's active links.
func (s *Service) eachLink(ctx context.Context, account uuid.UUID, fn func(gen.BillingCustomerLink) error) error {
	var after *uuid.UUID
	for {
		rows, err := s.db.GenDirectory().ListActiveCustomerLinksByAccount(ctx, gen.ListActiveCustomerLinksByAccountParams{AccountID: account, AfterMerchant: after, RowLimit: linkPage})
		if err != nil {
			return err
		}
		for _, row := range rows {
			if err := fn(row); err != nil {
				return err
			}
		}
		if len(rows) < linkPage {
			return nil
		}
		last := rows[len(rows)-1].MerchantID
		after = &last
	}
}

// Unlink ends the account's link: refused while the customer's
// subscriptions at the merchant still charge its cards. The link ends first,
// so the consent events that follow find nothing to end; then the merchant's
// clients lose the account's consent, so a merchant that signs its customers
// in with openrails.dev must ask again. Unlinking a link the account already
// ended finishes that, unless the account is linked there again. The
// customer, its orders and subscriptions stay.
func (s *Service) Unlink(ctx context.Context, account billing.NetworkAccountID, id billing.CustomerLinkID) (billing.CustomerLink, error) {
	directory := s.db.GenDirectory()
	row, err := directory.GetCustomerLinkOfAccount(ctx, gen.GetCustomerLinkOfAccountParams{AccountID: account.UUID(), ID: id.UUID()})
	switch {
	case db.IsNotFound(err):
		return billing.CustomerLink{}, errLinkNotFound
	case err != nil:
		return billing.CustomerLink{}, err
	}
	link := customernetwork.LinkOf(row)
	if link.Status == billing.CustomerLinkActive {
		if inUse, err := s.inUse(ctx, row); err != nil {
			return billing.CustomerLink{}, err
		} else if inUse {
			return billing.CustomerLink{}, errLinkInUse
		}
		if link, err = s.revoke(ctx, row, billing.CustomerLinkRevokedByCustomer); err != nil {
			return billing.CustomerLink{}, err
		}
	}
	if link.RevokedBy == nil || *link.RevokedBy != billing.CustomerLinkRevokedByCustomer {
		return link, nil
	}
	// A link made since owns the merchant's consents.
	_, err = directory.GetActiveCustomerLinkByAccount(ctx, gen.GetActiveCustomerLinkByAccountParams{AccountID: account.UUID(), MerchantID: row.MerchantID})
	switch {
	case err == nil:
		return link, nil
	case !db.IsNotFound(err):
		return billing.CustomerLink{}, err
	}
	if err := s.revokeConsents(ctx, account.String(), link.MerchantID); err != nil {
		return billing.CustomerLink{}, err
	}
	return link, nil
}

// ActiveLink is one of the account's links while it is active.
func (s *Service) ActiveLink(ctx context.Context, account billing.NetworkAccountID, id billing.CustomerLinkID) (billing.CustomerLink, bool, error) {
	row, err := s.db.GenDirectory().GetCustomerLinkOfAccount(ctx, gen.GetCustomerLinkOfAccountParams{AccountID: account.UUID(), ID: id.UUID()})
	switch {
	case db.IsNotFound(err):
		return billing.CustomerLink{}, false, nil
	case err != nil:
		return billing.CustomerLink{}, false, err
	}
	link := customernetwork.LinkOf(row)
	return link, link.Status == billing.CustomerLinkActive, nil
}

// inUse reports whether the link's customer has a subscription at its
// merchant that still charges, or will charge, its cards.
func (s *Service) inUse(ctx context.Context, row gen.BillingCustomerLink) (bool, error) {
	return s.db.Gen(ctx).CustomerHasLiveSubscriptions(ctx, gen.CustomerHasLiveSubscriptionsParams{MerchantID: row.MerchantID, CustomerID: row.CustomerID})
}

// revokeConsents revokes the account's consents to the merchant's clients
// and their refresh tokens (RFC 7009).
func (s *Service) revokeConsents(ctx context.Context, account string, mid billing.MerchantID) error {
	consents, err := s.ak.OAuthConsents(ctx, account)
	if err != nil {
		return fmt.Errorf("network: read consents: %w", err)
	}
	for _, c := range consents {
		if c.GroupID != mid.String() {
			continue
		}
		if err := s.ak.RevokeConsent(ctx, account, c.ClientID); err != nil && !errors.Is(err, iam.ErrOAuthConsentNotFound) {
			return fmt.Errorf("network: revoke consent: %w", err)
		}
	}
	return nil
}

// revoke ends a link and tells the merchant. A link already ended answers
// as it stands.
func (s *Service) revoke(ctx context.Context, row gen.BillingCustomerLink, by billing.CustomerLinkRevokedBy) (billing.CustomerLink, error) {
	var out billing.CustomerLink
	now := s.now().UTC()
	err := s.db.MerchantTx(merchant.WithID(ctx, billing.MerchantID(row.MerchantID)), func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		revoked, err := q.RevokeCustomerLink(ctx, gen.RevokeCustomerLinkParams{MerchantID: row.MerchantID, ID: row.ID, RevokedAt: now, RevokedBy: string(by)})
		if db.IsNotFound(err) {
			current, err := q.GetCustomerLink(ctx, gen.GetCustomerLinkParams{MerchantID: row.MerchantID, ID: row.ID})
			if db.IsNotFound(err) {
				return errLinkNotFound
			}
			out = customernetwork.LinkOf(current)
			return err
		}
		if err != nil {
			return err
		}
		out = customernetwork.LinkOf(revoked)
		return enqueue(ctx, q, revoked, billing.HostEventCustomerNetworkUnlinked, billing.CustomerNetworkHostEvent{AccountID: out.AccountID, RevokedBy: &by}, now)
	})
	return out, err
}

// applyEvent follows the account's retained AuthKit changes: a revoked consent to a
// merchant's client unlinks that merchant; accepting the network terms links
// the merchants whose client signs the account in as their own customer.
func (s *Service) applyEvent(ctx context.Context, e iam.Event) error {
	if e.TenantID != s.tenant {
		return fmt.Errorf("network: event belongs to a different account tenant")
	}
	switch e.Kind {
	case iam.EventOAuthConsentRevoked:
		mid, err := billing.ParseMerchantID(e.GroupID)
		account, aerr := uuid.Parse(e.UserID)
		if e.GroupID == "" || err != nil || aerr != nil {
			return nil
		}
		row, err := s.db.Gen(ctx).GetActiveCustomerLinkByAccount(ctx, gen.GetActiveCustomerLinkByAccountParams{AccountID: account, MerchantID: mid.UUID()})
		switch {
		case db.IsNotFound(err):
			return nil
		case err != nil:
			return err
		}
		// This fact may be replayed after a later, freshly proved link was
		// created. An earlier revocation cannot revoke that new relationship.
		if row.LinkedAt.After(e.OccurredAt) {
			return nil
		}
		_, err = s.revoke(ctx, row, billing.CustomerLinkRevokedByCustomer)
		return err
	case iam.EventUserAgreementAccepted:
		if e.Agreement != TermsAgreement {
			return nil
		}
		// Retained acceptances may predate soft deletion. Never recreate a
		// deleted account's links while its later purge is being completed.
		if _, err := s.ak.User(ctx, iam.UserByID(e.UserID)); err != nil {
			if errors.Is(err, iam.ErrUserNotFound) {
				return nil
			}
			return err
		}
		err := s.linkSignedIn(ctx, e.UserID)
		if errors.Is(err, iam.ErrUserNotFound) {
			return nil // A retained acceptance never recreates a purged account link.
		}
		return err
	}
	return nil
}

// linkSignedIn links the account's native customers at the merchants whose
// clients it lets act as itself (openrails:self): proven identity, never a
// match on a merchant's records.
func (s *Service) linkSignedIn(ctx context.Context, account string) error {
	subject, err := billing.ParseCustomerID(account)
	if err != nil {
		return nil
	}
	consents, err := s.ak.OAuthConsents(ctx, account)
	if err != nil {
		return fmt.Errorf("network: read consents: %w", err)
	}
	for _, c := range consents {
		mid, err := billing.ParseMerchantID(c.GroupID)
		if c.GroupID == "" || err != nil || !slices.Contains(c.Scopes, billing.ScopeSelf) {
			continue
		}
		mctx := merchant.WithID(ctx, mid)
		native, err := s.db.Gen(mctx).NativeCustomerExists(mctx, gen.NativeCustomerExistsParams{MerchantID: mid.UUID(), ID: subject.UUID()})
		if err != nil {
			return err
		}
		if !native {
			continue
		}
		if err := s.linkNative(mctx, mid, subject); err != nil {
			return err
		}
	}
	return nil
}

// OnPurge revokes a deleted account's links.
func (s *Service) OnPurge(ctx context.Context, d iam.UserDeletion) error {
	if d.TenantID != s.tenant {
		return nil
	}
	account, err := uuid.Parse(d.UserID)
	if err != nil {
		return nil
	}
	return s.eachLink(ctx, account, func(row gen.BillingCustomerLink) error {
		_, err := s.revoke(ctx, row, billing.CustomerLinkRevokedByAccountDeleted)
		return err
	})
}

// DeletionCheck refuses deleting an account while a linked customer's
// subscriptions still charge its cards.
func (s *Service) DeletionCheck(ctx context.Context, userID string) error {
	account, err := uuid.Parse(userID)
	if err != nil {
		return nil
	}
	return s.eachLink(ctx, account, func(row gen.BillingCustomerLink) error {
		inUse, err := s.inUse(ctx, row)
		if err == nil && inUse {
			return iam.RefuseDeletion("subscriptions")
		}
		return err
	})
}

// ConsentRevocationCheck refuses revoking a merchant client's consent, which
// unlinks the merchant, while the customer's subscriptions there still
// charge its cards.
func (s *Service) ConsentRevocationCheck(ctx context.Context, userID, clientID string) error {
	account, err := uuid.Parse(userID)
	if err != nil {
		return nil
	}
	consents, err := s.ak.OAuthConsents(ctx, userID)
	if err != nil {
		return fmt.Errorf("network: read consents: %w", err)
	}
	for _, c := range consents {
		mid, err := billing.ParseMerchantID(c.GroupID)
		if c.ClientID != clientID || err != nil {
			continue
		}
		row, err := s.db.GenDirectory().GetActiveCustomerLinkByAccount(ctx, gen.GetActiveCustomerLinkByAccountParams{AccountID: account, MerchantID: mid.UUID()})
		if db.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		inUse, err := s.inUse(ctx, row)
		if err == nil && inUse {
			return iam.RefuseConsentRevocation("subscriptions")
		}
		return err
	}
	return nil
}

// MerchantLeft revokes a deleted merchant's links: they are openrails.dev's,
// not the merchant's, and do not follow its records.
func (s *Service) MerchantLeft(ctx context.Context, mid billing.MerchantID) error {
	mctx := merchant.WithID(ctx, mid)
	for {
		rows, err := s.db.Gen(mctx).ListActiveCustomerLinksOfMerchant(mctx, gen.ListActiveCustomerLinksOfMerchantParams{MerchantID: mid.UUID(), RowLimit: linkPage})
		if err != nil {
			return err
		}
		for _, row := range rows {
			if _, err := s.revoke(mctx, row, billing.CustomerLinkRevokedByMerchantLeft); err != nil {
				return err
			}
		}
		if len(rows) < linkPage {
			log.WithContext(ctx).WithField("merchant_id", mid).Info("network: merchant's links revoked")
			return nil
		}
	}
}
