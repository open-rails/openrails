package controlplane

import (
	"context"
	"errors"
	"net/mail"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/credential"
	"github.com/open-rails/openrails/internal/db/gen"
)

// Federated grants (#1140): merchant roles an owner grants by email to users
// of the merchant's trusted issuers, who accept them with that verified email.
var (
	ErrFederatedGrantInvalidEmail = errors.New("controlplane: federated grant email is malformed")
	ErrFederatedGrantExists       = errors.New("controlplane: the email is already invited, or the user already holds a grant")
	ErrFederatedGrantNotFound     = errors.New("controlplane: federated grant not found")
	ErrFederatedGrantUnverified   = errors.New("controlplane: the access token carries no verified email")
)

// ListFederatedGrants is the merchant's grants, pending and accepted.
func (c *ControlPlane) ListFederatedGrants(ctx context.Context, mid billing.MerchantID) ([]billing.FederatedGrant, error) {
	rows, err := gen.New(c.pool).ListFederatedGrants(ctx, mid.UUID())
	if err != nil {
		return nil, err
	}
	out := make([]billing.FederatedGrant, 0, len(rows))
	for _, row := range rows {
		out = append(out, federatedGrant(row))
	}
	return out, nil
}

// CreateFederatedGrant invites email to the merchant with role.
func (c *ControlPlane) CreateFederatedGrant(ctx context.Context, mid billing.MerchantID, email, role string) (billing.FederatedGrant, error) {
	address, err := mail.ParseAddress(strings.TrimSpace(email))
	if err != nil || address.Name != "" {
		return billing.FederatedGrant{}, ErrFederatedGrantInvalidEmail
	}
	if _, ok := merchantRoleGrants(role); !ok {
		return billing.FederatedGrant{}, ErrUnknownMerchantRole
	}
	row, err := gen.New(c.pool).CreateFederatedGrant(ctx, gen.CreateFederatedGrantParams{MerchantID: mid.UUID(), Email: strings.ToLower(address.Address), Role: role})
	if uniqueViolation(err) {
		return billing.FederatedGrant{}, ErrFederatedGrantExists
	}
	if err != nil {
		return billing.FederatedGrant{}, err
	}
	return federatedGrant(row), nil
}

// FederatedGrant is one of the merchant's grants.
func (c *ControlPlane) FederatedGrant(ctx context.Context, mid billing.MerchantID, id billing.FederatedGrantID) (billing.FederatedGrant, error) {
	row, err := gen.New(c.pool).GetFederatedGrant(ctx, gen.GetFederatedGrantParams{MerchantID: mid.UUID(), ID: id.UUID()})
	if errors.Is(err, pgx.ErrNoRows) {
		return billing.FederatedGrant{}, ErrFederatedGrantNotFound
	}
	if err != nil {
		return billing.FederatedGrant{}, err
	}
	return federatedGrant(row), nil
}

// RevokeFederatedGrant deletes a grant, pending or accepted.
func (c *ControlPlane) RevokeFederatedGrant(ctx context.Context, mid billing.MerchantID, id billing.FederatedGrantID) error {
	n, err := gen.New(c.pool).DeleteFederatedGrant(ctx, gen.DeleteFederatedGrantParams{MerchantID: mid.UUID(), ID: id.UUID()})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrFederatedGrantNotFound
	}
	return nil
}

// PendingFederatedGrants are the grants user's verified email may accept on
// the merchants its issuer is trusted for.
func (c *ControlPlane) PendingFederatedGrants(ctx context.Context, user *credential.ResourceUser) ([]billing.FederatedInvite, error) {
	out := []billing.FederatedInvite{}
	email, ok := grantEmail(user)
	if !ok {
		return out, nil
	}
	rows, err := gen.New(c.pool).ListPendingFederatedGrantsForEmail(ctx, gen.ListPendingFederatedGrantsForEmailParams{Email: email, MerchantIds: boundIDs(user)})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out = append(out, billing.FederatedInvite{ID: billing.FederatedGrantID(row.ID), Merchant: boundRef(user, row.MerchantID), Role: row.Role})
	}
	return out, nil
}

// AcceptFederatedGrant binds a pending grant offered to user's verified
// email to user, and answers the merchant it now reaches.
func (c *ControlPlane) AcceptFederatedGrant(ctx context.Context, user *credential.ResourceUser, id billing.FederatedGrantID) (billing.UserMerchant, error) {
	email, ok := grantEmail(user)
	if !ok {
		return billing.UserMerchant{}, ErrFederatedGrantUnverified
	}
	q := gen.New(c.pool)
	pending, err := q.ListPendingFederatedGrantsForEmail(ctx, gen.ListPendingFederatedGrantsForEmailParams{Email: email, MerchantIds: boundIDs(user)})
	if err != nil {
		return billing.UserMerchant{}, err
	}
	for _, p := range pending {
		if p.ID != id.UUID() {
			continue
		}
		row, err := q.AcceptFederatedGrant(ctx, gen.AcceptFederatedGrantParams{Issuer: user.Issuer, Subject: user.Subject, MerchantID: p.MerchantID, ID: p.ID, Email: email})
		switch {
		case uniqueViolation(err):
			return billing.UserMerchant{}, ErrFederatedGrantExists
		case errors.Is(err, pgx.ErrNoRows):
			return billing.UserMerchant{}, ErrFederatedGrantNotFound
		case err != nil:
			return billing.UserMerchant{}, err
		}
		grants, _ := merchantRoleGrants(row.Role)
		ref := boundRef(user, row.MerchantID)
		perms := credential.IntersectPermissions(grants, user.Ceiling)
		return billing.UserMerchant{ID: ref.ID, Slug: ref.Slug, DisplayName: ref.DisplayName, Role: merchantRoleFor(perms), Permissions: perms}, nil
	}
	return billing.UserMerchant{}, ErrFederatedGrantNotFound
}

// subjectGrants are the roles' grants a trusted issuer's user accepted, by
// merchant.
func (c *ControlPlane) subjectGrants(ctx context.Context, issuer, subject string, merchants []billing.MerchantID) (map[billing.MerchantID][]string, error) {
	ids := make([]uuid.UUID, 0, len(merchants))
	for _, m := range merchants {
		ids = append(ids, m.UUID())
	}
	rows, err := gen.New(c.pool).ListFederatedGrantsForSubject(ctx, gen.ListFederatedGrantsForSubjectParams{Issuer: issuer, Subject: subject, MerchantIds: ids})
	if err != nil {
		return nil, err
	}
	out := map[billing.MerchantID][]string{}
	for _, row := range rows {
		grants, _ := merchantRoleGrants(row.Role)
		out[billing.MerchantID(row.MerchantID)] = append(out[billing.MerchantID(row.MerchantID)], grants...)
	}
	return out, nil
}

func grantEmail(user *credential.ResourceUser) (string, bool) {
	if user == nil || user.Machine || !user.EmailVerified {
		return "", false
	}
	email := strings.ToLower(strings.TrimSpace(user.Email))
	return email, email != ""
}

func boundIDs(user *credential.ResourceUser) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(user.Bound))
	for _, ref := range user.Bound {
		ids = append(ids, ref.ID.UUID())
	}
	return ids
}

func boundRef(user *credential.ResourceUser, id uuid.UUID) billing.MerchantRef {
	for _, ref := range user.Bound {
		if ref.ID.UUID() == id {
			return ref
		}
	}
	return billing.MerchantRef{ID: billing.MerchantID(id)}
}

func federatedGrant(row gen.BillingFederatedGrant) billing.FederatedGrant {
	return billing.FederatedGrant{
		ID: billing.FederatedGrantID(row.ID), Email: row.Email, Role: row.Role,
		Issuer: row.Issuer, Subject: row.Subject, AcceptedAt: row.AcceptedAt, CreatedAt: row.CreatedAt,
	}
}

func uniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
