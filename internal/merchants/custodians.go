package merchants

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#880: custody is Layer B state exactly like a PSP is. A custodian is
// declared once per merchant and referenced by every PSP whose gateway charges
// the cards it holds, so this file is the ONE place a custodian row becomes a
// scope — whether the row got there through the mode-1 manifest or a mode-2
// API write.

// ErrCustodianNotDeclared reports that a PSP references a custodian key no
// custodians row carries. It is always fail-closed: a PSP whose custody cannot
// be resolved must not arm, because charging it as though the gateway held the
// card is the wrong charge, not a degraded one.
var ErrCustodianNotDeclared = errors.New("merchants: custodian is not declared")

// CustodianScope is one resolved custodian row.
type CustodianScope struct {
	ID uuid.UUID
	// Key is the merchant's name for it (the value a PSP references).
	Key string
	// Kind is the vendor (custodians registry): basis_theory today.
	Kind        string
	Environment string
	// AccountID is the custodian-native tenant identity.
	AccountID string
	Settings  map[string]any
	Archived  bool
	// CredentialVersions is the rotation watermark per credential key
	// (or#812), read from the custodians row. Absent/zero = no floor.
	CredentialVersions map[string]int
}

// SecretRef returns the custodian-scoped secret name for key together with the
// rotation version floor recorded on this custodian row — the same versioned
// read every PSP credential goes through (or#812), so a custodial key rotated
// on one node is effective on every node the instant it commits.
func (c CustodianScope) SecretRef(key string) (SecretRef, error) {
	name, err := CustodianSecretName(c.Kind, c.Environment, c.AccountID, key)
	if err != nil {
		return SecretRef{}, err
	}
	return SecretRef{Name: name, MinVersion: c.CredentialVersions[NormalizeCredentialVersionKey(key)]}, nil
}

// CustodianIdentity is the routing tuple an inbound custodian webhook resolves
// to: which merchant owns the tenant id the event carries.
type CustodianIdentity struct {
	ID          uuid.UUID
	MerchantID  merchant.ID
	Key         string
	Kind        string
	Environment string
	AccountID   string
}

func custodianScopeFrom(row gen.OpenrailsCustodian) CustodianScope {
	return CustodianScope{
		ID:          row.ID,
		Key:         strings.TrimSpace(row.Key),
		Kind:        row.Kind,
		Environment: row.Environment,
		AccountID:   row.AccountID,
		Settings:    decodeCustodianSettings(row.Settings),
		Archived:    row.Archived,
		// or#812: the floors ride on the row every resolution already re-reads.
		CredentialVersions: decodeCredentialVersions(row.CredentialVersions),
	}
}

func decodeCredentialVersions(raw []byte) map[string]int {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]int
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

func decodeCustodianSettings(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// CustodianScopeByKey resolves a merchant's custodian by its declared key.
func (s *Service) CustodianScopeByKey(ctx context.Context, id merchant.ID, key string) (CustodianScope, bool, error) {
	key = strings.TrimSpace(key)
	if s == nil || s.pool == nil || id.IsZero() || key == "" {
		return CustodianScope{}, false, nil
	}
	var row gen.OpenrailsCustodian
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = gen.New(tx).GetCustodianByKey(ctx, gen.GetCustodianByKeyParams{
			MerchantID: id.UUID(),
			Key:        key,
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return CustodianScope{}, false, nil
	}
	if err != nil {
		return CustodianScope{}, false, fmt.Errorf("load custodian %q: %w", key, err)
	}
	return custodianScopeFrom(row), true, nil
}

// CustodianScopeByID resolves a custodian by its row id — the shape a PSP's
// custodian_id reference takes.
func (s *Service) CustodianScopeByID(ctx context.Context, id merchant.ID, custodianID uuid.UUID) (CustodianScope, bool, error) {
	if s == nil || s.pool == nil || id.IsZero() || custodianID == uuid.Nil {
		return CustodianScope{}, false, nil
	}
	var row gen.OpenrailsCustodian
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = gen.New(tx).GetCustodian(ctx, custodianID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return CustodianScope{}, false, nil
	}
	if err != nil {
		return CustodianScope{}, false, fmt.Errorf("load custodian %s: %w", custodianID, err)
	}
	return custodianScopeFrom(row), true, nil
}

// CustodianScopeByIdentity resolves a merchant's custodian by its vendor
// identity (kind + environment + tenant id) — the shape a custodian webhook
// carries once the merchant is already known.
func (s *Service) CustodianScopeByIdentity(ctx context.Context, id merchant.ID, kind, environment, accountID string) (CustodianScope, bool, error) {
	kind = custodians.Normalize(kind)
	environment = normalizeProviderSecretEnvironment(environment)
	accountID = strings.TrimSpace(accountID)
	if s == nil || s.pool == nil || id.IsZero() || kind == "" || accountID == "" {
		return CustodianScope{}, false, nil
	}
	if environment == "" {
		return CustodianScope{}, false, errors.New("merchants: custodian environment must be live or test")
	}
	var row gen.OpenrailsCustodian
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = gen.New(tx).GetCustodianByIdentity(ctx, gen.GetCustodianByIdentityParams{
			MerchantID:  id.UUID(),
			Kind:        kind,
			Environment: &environment,
			AccountID:   accountID,
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return CustodianScope{}, false, nil
	}
	if err != nil {
		return CustodianScope{}, false, fmt.Errorf("load custodian %s/%s: %w", kind, accountID, err)
	}
	return custodianScopeFrom(row), true, nil
}

// ListCustodians lists a merchant's declared custodians.
func (s *Service) ListCustodians(ctx context.Context, id merchant.ID) ([]CustodianScope, error) {
	if s == nil || s.pool == nil || id.IsZero() {
		return nil, nil
	}
	var rows []gen.OpenrailsCustodian
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = gen.New(tx).ListCustodiansForMerchant(ctx, id.UUID())
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list custodians: %w", err)
	}
	out := make([]CustodianScope, 0, len(rows))
	for _, row := range rows {
		out = append(out, custodianScopeFrom(row))
	}
	return out, nil
}

// UpsertCustodian converges ONE declared custodian. Both ingestion planes go
// through it, and both validate through config.ValidateCustodianEntry first —
// so a value the manifest accepts is never one an API write silently drops.
func (s *Service) UpsertCustodian(ctx context.Context, id merchant.ID, entry config.CustodianEntry, environment string) (CustodianScope, error) {
	if s == nil || s.pool == nil || id.IsZero() {
		return CustodianScope{}, errors.New("merchants: custodian upsert requires a merchant")
	}
	if err := config.ValidateCustodianEntry(entry); err != nil {
		return CustodianScope{}, err
	}
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return CustodianScope{}, errors.New("merchants: custodian environment must be live or test")
	}
	settings := entry.Settings
	if settings == nil {
		settings = map[string]any{}
	}
	settingsJSON, err := json.Marshal(settings)
	if err != nil {
		return CustodianScope{}, fmt.Errorf("encode custodian settings: %w", err)
	}
	// or#812: a caller that rotated a credential records its new version here;
	// the upsert merges floors forward and never clears one it was not given.
	versionsJSON, err := json.Marshal(nonNilVersions(entry.CredentialVersions))
	if err != nil {
		return CustodianScope{}, fmt.Errorf("encode custodian credential versions: %w", err)
	}
	archived := entry.Archived
	kind := custodians.Normalize(entry.Kind)
	if err := AssertCustodianUnowned(ctx, gen.New(s.pool), id.UUID(), kind, environment, entry.AccountID); err != nil {
		return CustodianScope{}, err
	}
	var row gen.OpenrailsCustodian
	err = s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = gen.New(tx).UpsertCustodian(ctx, gen.UpsertCustodianParams{
			MerchantID:         id.UUID(),
			Key:                strings.TrimSpace(entry.Key),
			Kind:               kind,
			Environment:        &environment,
			AccountID:          strings.TrimSpace(entry.AccountID),
			Settings:           settingsJSON,
			Archived:           &archived,
			CredentialVersions: versionsJSON,
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The upsert's WHERE clause guards the global identity; the preflight
		// above should already have named the owner.
		return CustodianScope{}, fmt.Errorf("custodian %q (%s): tenant %s: %w", entry.Key, kind, entry.AccountID, ErrCustodianOwnedByAnotherMerchant)
	}
	if err != nil {
		return CustodianScope{}, fmt.Errorf("upsert custodian %q: %w", entry.Key, err)
	}
	return custodianScopeFrom(row), nil
}

func nonNilVersions(in map[string]int) map[string]int {
	if in == nil {
		return map[string]int{}
	}
	return in
}

// ErrCustodianOwnedByAnotherMerchant reports that the declared (kind,
// environment, account_id) identity already belongs to a different merchant.
// The unique index would reject the write anyway — but under RLS the
// conflicting row is invisible, so without this preflight the operator sees an
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
	if row.MerchantID == nil || *row.MerchantID == merchantID {
		return nil
	}
	return fmt.Errorf("custodian %s tenant %s (%s): %w", kind, accountID, environment, ErrCustodianOwnedByAnotherMerchant)
}

// ResolveCustodianByIdentity resolves the merchant a CUSTODIAN's tenant
// identity belongs to (or#880). Custody is not a rail, so a Basis Theory
// webhook cannot route through the rail directory — but the problem is
// identical (an inbound event with a global provider-side id and no merchant
// context) and so is the answer: the SECURITY DEFINER directory function,
// which raises rather than returning empty when it cannot read across
// merchants. It resolves the CUSTODIAN, never "the" PSP: one custodian may
// back several, so that was never a well-defined question.
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
	if row.ID == nil || row.MerchantID == nil {
		return CustodianIdentity{}, false, nil
	}
	return CustodianIdentity{
		ID:          *row.ID,
		MerchantID:  merchant.ID(*row.MerchantID),
		Key:         derefString(row.Key),
		Kind:        derefString(row.Kind),
		Environment: derefString(row.Environment),
		AccountID:   derefString(row.AccountID),
	}, true, nil
}
