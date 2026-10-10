package merchants

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/gen"
)

// ErrCustodianNotDeclared reports that a PSP references a custodian key no
// custodian document carries. It is always fail-closed: a PSP whose custody
// cannot be resolved must not arm, because charging it as though the gateway
// held the card is the wrong charge, not a degraded one.
var ErrCustodianNotDeclared = errors.New("merchants: custodian is not declared")

// CustodianScope is one custodian: its identity joined with its document.
type CustodianScope struct {
	ID uuid.UUID
	// Key is the merchant's name for it (the value a PSP references).
	Key string
	// Kind is the vendor (custodians registry).
	Kind        string
	Environment string
	// AccountID is the custodian-native tenant identity.
	AccountID string
	Settings  map[string]any
	// Archived custodians take no new arrangement; one no document names is
	// archived.
	Archived bool
	Revision int64
	secrets  map[string]string
}

// SecretRef names the custodian's credential slot key.
func (c CustodianScope) SecretRef(key string) (SecretRef, error) {
	name, err := CustodianSecretName(c.Kind, c.Environment, c.AccountID, key)
	if err != nil {
		return SecretRef{}, err
	}
	return SecretRef{Name: name}, nil
}

// Secret is the value of credential key, "" when the document holds none.
func (c CustodianScope) Secret(key string) string {
	return strings.TrimSpace(c.secrets[NormalizeCredentialVersionKey(key)])
}

// CustodianIdentity is the routing tuple an inbound custodian webhook resolves
// to: which merchant owns the tenant id the event carries.
type CustodianIdentity struct {
	ID          uuid.UUID
	MerchantID  billing.MerchantID
	Key         string
	Kind        string
	Environment string
	AccountID   string
}

// CustodianScopeByKey resolves a merchant's custodian by its key.
func (s *Service) CustodianScopeByKey(ctx context.Context, id billing.MerchantID, key string) (CustodianScope, bool, error) {
	key = strings.ToLower(strings.TrimSpace(key))
	if s == nil || s.pool == nil || id.IsZero() || key == "" {
		return CustodianScope{}, false, nil
	}
	j, err := s.load(ctx, id)
	if err != nil {
		return CustodianScope{}, false, err
	}
	for _, c := range j.custodians {
		if strings.EqualFold(c.Key, key) {
			return c, true, nil
		}
	}
	return CustodianScope{}, false, nil
}

// CustodianScopeByID resolves a custodian by its identity id.
func (s *Service) CustodianScopeByID(ctx context.Context, id billing.MerchantID, custodianID uuid.UUID) (CustodianScope, bool, error) {
	if s == nil || s.pool == nil || id.IsZero() || custodianID == uuid.Nil {
		return CustodianScope{}, false, nil
	}
	j, err := s.load(ctx, id)
	if err != nil {
		return CustodianScope{}, false, err
	}
	c, ok := j.custodianByID(custodianID)
	return c, ok, nil
}

// CustodianScopeByIdentity resolves a merchant's custodian by its vendor
// identity (kind + environment + tenant id) — the shape a custodian webhook
// carries once the merchant is already known.
func (s *Service) CustodianScopeByIdentity(ctx context.Context, id billing.MerchantID, kind, environment, accountID string) (CustodianScope, bool, error) {
	kind = custodians.Normalize(kind)
	environment = normalizeProviderSecretEnvironment(environment)
	accountID = strings.TrimSpace(accountID)
	if s == nil || s.pool == nil || id.IsZero() || kind == "" || accountID == "" {
		return CustodianScope{}, false, nil
	}
	if environment == "" {
		return CustodianScope{}, false, errors.New("merchants: custodian environment must be live or test")
	}
	j, err := s.load(ctx, id)
	if err != nil {
		return CustodianScope{}, false, err
	}
	for _, c := range j.custodians {
		if c.Kind == kind && c.Environment == environment && c.AccountID == accountID {
			return c, true, nil
		}
	}
	return CustodianScope{}, false, nil
}

// ListCustodians lists a merchant's custodians.
func (s *Service) ListCustodians(ctx context.Context, id billing.MerchantID) ([]CustodianScope, error) {
	if s == nil || s.pool == nil || id.IsZero() {
		return nil, nil
	}
	j, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	return j.custodians, nil
}

// ErrCustodianOwnedByAnotherMerchant reports that the declared (kind,
// environment, account_id) identity already belongs to a different merchant.
// The unique index would reject the write anyway — but the conflicting row
// belongs to another merchant, so without this preflight the operator sees an
// opaque constraint violation instead of "that tenant is not yours" (#650).
var ErrCustodianOwnedByAnotherMerchant = errors.New("merchants: custodian tenant is declared by another merchant")

// AssertCustodianUnowned is the custody sibling of AssertPSPUnowned: it reads
// the cross-merchant directory function before an upsert claims an identity.
func AssertCustodianUnowned(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, kind, environment, accountID string) error {
	accountID = strings.TrimSpace(accountID)
	if q == nil || accountID == "" {
		return nil
	}
	kind = custodians.Normalize(kind)
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return errors.New("merchants: custodian environment must be live or test")
	}
	row, err := q.ResolveCustodianOwnerByIdentity(ctx, gen.ResolveCustodianOwnerByIdentityParams{
		Kind:        kind,
		Environment: &environment,
		AccountID:   accountID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.MerchantID == merchantID {
		return nil
	}
	return fmt.Errorf("custodian %s tenant %s (%s): %w", kind, accountID, environment, ErrCustodianOwnedByAnotherMerchant)
}

// ResolveCustodianByIdentity resolves the merchant a CUSTODIAN's tenant
// identity belongs to (or#880). Custody is not a rail, so a Basis Theory
// webhook cannot route through the rail directory — but the problem is
// identical (an inbound event with a global provider-side id and no merchant
// context) and so is the answer: a lookup by the global identity key. It
// resolves the CUSTODIAN, never "the" PSP: one custodian may back several.
func (s *Service) ResolveCustodianByIdentity(ctx context.Context, kind, environment, accountID string) (CustodianIdentity, bool, error) {
	if s == nil || s.pool == nil || strings.TrimSpace(accountID) == "" {
		return CustodianIdentity{}, false, nil
	}
	kind = custodians.Normalize(kind)
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return CustodianIdentity{}, false, errors.New("merchants: custodian environment must be live or test")
	}
	env := environment
	row, err := gen.New(s.pool).ResolveCustodianOwnerByIdentity(ctx, gen.ResolveCustodianOwnerByIdentityParams{
		Kind:        kind,
		Environment: &env,
		AccountID:   strings.TrimSpace(accountID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return CustodianIdentity{}, false, nil
	}
	if err != nil {
		return CustodianIdentity{}, false, err
	}
	return CustodianIdentity{
		ID:          row.ID,
		MerchantID:  billing.MerchantID(row.MerchantID),
		Key:         row.Key,
		Kind:        row.Kind,
		Environment: row.Environment,
		AccountID:   row.AccountID,
	}, true, nil
}

// CustodianRoutePSPs are the live PSPs of rail, in the custodian's
// environment, that reach the custodian: the PSPs a card it holds can be
// charged through, oldest first. More than one means routing has no single
// answer.
func (s *Service) CustodianRoutePSPs(ctx context.Context, id billing.MerchantID, rail string, custodianID uuid.UUID) ([]uuid.UUID, error) {
	j, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	custodian, ok := j.custodianByID(custodianID)
	if !ok {
		return nil, nil
	}
	var out []uuid.UUID
	for _, p := range j.psps {
		if !p.Archived && p.Rail == rail && p.Environment == custodian.Environment && p.CustodianID != nil && *p.CustodianID == custodianID {
			out = append(out, p.ID)
		}
	}
	return out, nil
}
