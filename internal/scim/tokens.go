package scim

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// TokenPrefix starts every provisioning token OpenRails mints, so secret
// scanners recognize one.
const TokenPrefix = "orscim_"

// MinDeclaredToken is the shortest token a merchant declaration may name.
const MinDeclaredToken = 32

func tokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Tokens are the merchant's provisioning tokens.
type Tokens struct{ DB *db.DB }

// ErrTokenNotFound is a token id the merchant does not hold.
var ErrTokenNotFound = apperr.New(http.StatusNotFound, "resource_not_found", "provisioning token not found")

func tokenOf(row gen.BillingProvisioningToken) billing.ProvisioningToken {
	return billing.ProvisioningToken{
		ID: billing.ProvisioningTokenID(row.ID), Name: row.Name, Declared: row.Declared,
		CreatedAt: row.CreatedAt.UTC(), LastUsedAt: utc(row.LastUsedAt),
	}
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// List is the merchant's tokens, oldest first, or those ids names.
func (t Tokens) List(ctx context.Context, mid billing.MerchantID, ids []billing.ProvisioningTokenID) ([]billing.ProvisioningToken, error) {
	rows, err := t.DB.Gen(ctx).ListProvisioningTokens(ctx, mid.UUID())
	if err != nil {
		return nil, err
	}
	out := make([]billing.ProvisioningToken, 0, len(rows))
	for _, row := range rows {
		if ids == nil || slices.Contains(ids, billing.ProvisioningTokenID(row.ID)) {
			out = append(out, tokenOf(row))
		}
	}
	return out, nil
}

// Create mints a token; its secret is in the answer only.
func (t Tokens) Create(ctx context.Context, mid billing.MerchantID, name string) (billing.CreatedProvisioningToken, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 128 {
		return billing.CreatedProvisioningToken{}, apperr.Invalidf("name must be 1 to 128 bytes").WithParam("name")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return billing.CreatedProvisioningToken{}, err
	}
	token := TokenPrefix + base64.RawURLEncoding.EncodeToString(secret)
	row, err := t.DB.Gen(ctx).CreateProvisioningToken(ctx, gen.CreateProvisioningTokenParams{MerchantID: mid.UUID(), Name: name, TokenSha256: tokenHash(token)})
	if err != nil {
		return billing.CreatedProvisioningToken{}, err
	}
	return billing.CreatedProvisioningToken{ProvisioningToken: tokenOf(row), Token: token}, nil
}

// Delete revokes a token: the next request presenting it is refused.
func (t Tokens) Delete(ctx context.Context, mid billing.MerchantID, id billing.ProvisioningTokenID) error {
	n, err := t.DB.Gen(ctx).DeleteProvisioningToken(ctx, gen.DeleteProvisioningTokenParams{MerchantID: mid.UUID(), ID: id.UUID()})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrTokenNotFound
	}
	return nil
}

// Declare makes token the merchant's declared token, replacing the one
// declared before; an empty token removes it.
func Declare(ctx context.Context, q *gen.Queries, mid billing.MerchantID, token string) error {
	token = strings.TrimSpace(token)
	var keep []byte
	if token != "" {
		if len(token) < MinDeclaredToken {
			return fmt.Errorf("secrets.scim_token must be at least %d characters", MinDeclaredToken)
		}
		keep = tokenHash(token)
	}
	if err := q.DeleteDeclaredProvisioningTokens(ctx, gen.DeleteDeclaredProvisioningTokensParams{MerchantID: mid.UUID(), KeepSha256: keep}); err != nil {
		return err
	}
	if keep == nil {
		return nil
	}
	return q.DeclareProvisioningToken(ctx, gen.DeclareProvisioningTokenParams{MerchantID: mid.UUID(), TokenSha256: keep})
}

// Resolve is the merchant whose token r presents as its bearer;
// ErrUnauthorized when it presents none OpenRails holds.
func (t Tokens) Resolve(ctx context.Context, token string) (billing.MerchantID, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return billing.MerchantID{}, ErrUnauthorized
	}
	q := t.DB.GenDirectory()
	row, err := q.ResolveProvisioningToken(ctx, tokenHash(token))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return billing.MerchantID{}, ErrUnauthorized
	case err != nil:
		return billing.MerchantID{}, err
	}
	if err := q.TouchProvisioningToken(ctx, gen.TouchProvisioningTokenParams{MerchantID: row.MerchantID, ID: row.ID}); err != nil {
		return billing.MerchantID{}, err
	}
	return billing.MerchantID(row.MerchantID), nil
}

// BearerToken is the credential of an Authorization: Bearer header.
func BearerToken(r *http.Request) string {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") {
		return fields[1]
	}
	return ""
}
