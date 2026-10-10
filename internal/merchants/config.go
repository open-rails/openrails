package merchants

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
)

// joined is one merchant's identities joined with its documents.
type joined struct {
	set        merchantdocs.Set
	psps       []PSPScope // oldest first
	custodians []CustodianScope
}

// load joins the merchant's PSP and custodian identities with its documents.
func (s *Service) load(ctx context.Context, id billing.MerchantID) (joined, error) {
	if s == nil || s.pool == nil || s.config == nil {
		return joined{}, errors.New("merchants: merchant configuration unavailable")
	}
	if id.IsZero() {
		return joined{}, errors.New("merchants: a merchant is required")
	}
	set, err := s.config.Get(ctx, id)
	if err != nil {
		return joined{}, err
	}
	var pspRows []gen.BillingPsp
	var custodianRows []gen.BillingCustodian
	if err := s.database.RunInMerchantConn(merchant.WithID(ctx, id), func(ctx context.Context) error {
		q := s.database.Gen(ctx)
		var err error
		if pspRows, err = q.ListPSPsForMerchant(ctx, id.UUID()); err != nil {
			return err
		}
		custodianRows, err = q.ListCustodiansForMerchant(ctx, id.UUID())
		return err
	}); err != nil {
		return joined{}, fmt.Errorf("merchants: load PSP identities: %w", err)
	}
	out := joined{set: set}
	custodianIDs := map[string]uuid.UUID{}
	for _, row := range custodianRows {
		scope := CustodianScope{ID: row.ID, Key: row.Key, Kind: row.Kind, Environment: row.Environment, AccountID: row.AccountID, Archived: true}
		if doc, ok := set.Custodians[strings.ToLower(row.Key)]; ok && custodianDocNames(doc.Value, row) {
			scope.Settings, scope.Archived, scope.Revision, scope.secrets = doc.Value.Settings, doc.Value.Archived, doc.Revision, doc.Value.Secrets
			custodianIDs[strings.ToLower(row.Key)] = row.ID
		}
		out.custodians = append(out.custodians, scope)
	}
	for _, row := range pspRows {
		scope := PSPScope{
			ID: row.ID, Rail: row.Rail, Environment: row.Environment, AccountID: row.AccountID, Key: row.Key,
			Archived: true, DuplicateAccount: row.CredentialDuplicateAt != nil,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, ValidatedAt: row.CredentialsValidatedAt,
		}
		if row.PendingSignerPublicKey != nil {
			scope.SignerChange = *row.PendingSignerPublicKey
		}
		if row.WebhookEndpointID != nil {
			scope.WebhookEndpointID = *row.WebhookEndpointID
		}
		if doc, ok := set.PSPs[strings.ToLower(row.Key)]; ok && row.SupersededAt == nil && pspDocNames(doc.Value, row) {
			p := doc.Value
			scope.Settings, scope.Archived, scope.Signer, scope.secrets = p.Settings, p.Archived, p.Signer, normalizedKeys(p.Secrets)
			scope.Revision = doc.Revision
			if !doc.UpdatedAt.IsZero() {
				scope.UpdatedAt = doc.UpdatedAt
			}
			scope.WebhookOverlapUntil = overlapUntil(p.Settings)
			if key := strings.ToLower(strings.TrimSpace(p.Custodian)); key != "" {
				scope.Custodian = key
				if cid, ok := custodianIDs[key]; ok {
					scope.CustodianID = &cid
				}
			}
		}
		out.psps = append(out.psps, scope)
	}
	return out, nil
}

func pspDocNames(p merchantdocs.PSP, row gen.BillingPsp) bool {
	return strings.EqualFold(p.Rail, row.Rail) && p.Environment == row.Environment && strings.TrimSpace(p.AccountID) == row.AccountID
}

func custodianDocNames(c merchantdocs.Custodian, row gen.BillingCustodian) bool {
	return strings.EqualFold(c.Kind, row.Kind) && c.Environment == row.Environment && strings.TrimSpace(c.AccountID) == row.AccountID
}

func normalizedKeys(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[NormalizeCredentialVersionKey(k)] = v
	}
	return out
}

// overlapUntil reads the declared end of a rotated-out webhook secret's
// overlap; no end, or an unparsable one, refuses the old secret.
func overlapUntil(settings map[string]any) time.Time {
	raw, _ := settings[WebhookOverlapExpiresKey].(string)
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}
	}
	return at.UTC()
}

// live are the PSPs that take new work in environment on rail ("" any),
// newest first.
func (j joined) live(rail, environment string) []PSPScope {
	var out []PSPScope
	for _, p := range slices.Backward(j.psps) {
		if !p.Archived && p.Environment == environment && (rail == "" || p.Rail == rail) {
			out = append(out, p)
		}
	}
	return out
}

func (j joined) byID(id uuid.UUID) (PSPScope, bool) {
	for _, p := range j.psps {
		if p.ID == id {
			return p, true
		}
	}
	return PSPScope{}, false
}

func (j joined) custodianByID(id uuid.UUID) (CustodianScope, bool) {
	for _, c := range j.custodians {
		if c.ID == id {
			return c, true
		}
	}
	return CustodianScope{}, false
}
