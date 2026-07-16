package merchants

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"

	"github.com/open-rails/openrails/pkg/merchant"
)

// MerchantStatus mirrors openrails.merchants.status.
type MerchantStatus string

const (
	StatusActive  MerchantStatus = "active"
	StatusDeleted MerchantStatus = "deleted"
)

// Merchant is the directory view of a row in openrails.merchants.
type Merchant struct {
	ID                merchant.ID
	Slug              string
	Status            MerchantStatus
	PermissionGroupID string // The merchant's own AuthKit permission-group id (#567).
}

// ProvisionRequest parameterizes merchant provisioning.
type ProvisionRequest struct {
	// Slug is the stable merchant slug (used in routes and merchant resolution).
	Slug string `json:"slug"`
	// PermissionGroupID is the merchant's own AuthKit permission-group id (#567).
	// Required for control-plane provisioning; embedded/no-AuthKit registration
	// uses internal/db.RegisterMerchant.
	PermissionGroupID string `json:"permission_group_id"`
}

// ErrMerchantNotFound indicates no openrails.merchants row matched.
var ErrMerchantNotFound = errors.New("merchants: merchant not found")

// ErrPermissionGroupRequired indicates control-plane merchant provisioning
// tried to create a merchant namespace without its authkit permission-group id.
var ErrPermissionGroupRequired = errors.New("merchants: permission group required")

// ErrExportRequired is returned by Delete when no completed export exists for the
// merchant (export-before-delete is enforced).
var ErrExportRequired = errors.New("merchants: export required before delete")

// Service is the merchant provisioning + lifecycle service (issue #225). It owns
// the openrails.merchants directory rows (billing buckets) and per-merchant
// secrets. Control-plane callers create/resolve the AuthKit permission-group and
// pass its id explicitly; this service never creates AuthKit authority itself.
type Service struct {
	pool    *db.Pool
	secrets MerchantSecretStore
	// providerEnvironment is the deployment posture (#681): test under
	// test_mode, live otherwise. Scoped credential lookups resolve
	// rail_merchant_accounts rows in THIS environment only.
	providerEnvironment string
	// nmiProbeV5BaseURL is a test-only seam: overrides the base URL the #348
	// test_mode arm-time probe (refuseLiveNMIUnderTestMode) hits, so tests can
	// point it at a fake gateway instead of the real NMI API. Empty in
	// production — the probe uses nmi.NewClient's documented default.
	nmiProbeV5BaseURL string
}

// NewService builds the lifecycle service. pool is required (it owns the merchant
// directory). secrets may be nil (credential management disabled).
// providerEnvironment is the deployment's provider-account environment —
// derive it via config.ExpectedProviderEnvironment(cfg.IsTestMode()).
func NewService(pool *db.Pool, secrets MerchantSecretStore, providerEnvironment string) (*Service, error) {
	if pool == nil {
		return nil, errors.New("merchants: pgx pool is required")
	}
	env := normalizeProviderSecretEnvironment(providerEnvironment)
	if env == "" {
		return nil, fmt.Errorf("merchants: provider environment must be live or test, got %q", providerEnvironment)
	}
	return &Service{pool: pool, secrets: secrets, providerEnvironment: env}, nil
}

// NewDirectoryService builds a directory-only Service: merchant provisioning +
// lookup over openrails.merchants, with no secret store and no provider-account
// environment (scoped credential lookups are unavailable). It is the lifecycle
// slice the control-plane provisioning seam needs (#738).
func NewDirectoryService(pool *db.Pool) (*Service, error) {
	if pool == nil {
		return nil, errors.New("merchants: pgx pool is required")
	}
	return &Service{pool: pool}, nil
}

// NewSecretManagementService builds a secret-management-only Service. It is for
// runtimes/tests that only need credential list/write/delete/validate behavior;
// lifecycle methods such as Provision still require NewService with a DB pool.
func NewSecretManagementService(secrets MerchantSecretStore) (*Service, error) {
	if secrets == nil {
		return nil, errors.New("merchants: secret store is required")
	}
	return &Service{secrets: secrets}, nil
}

// Secrets exposes the per-merchant secret store (may be nil).
func (s *Service) Secrets() MerchantSecretStore { return s.secrets }

// Provision idempotently provisions a merchant:
//
//  1. create/ensure the openrails.merchants namespace row (resolve by slug),
//  2. record the merchant permission-group id supplied by the caller.
//
// Re-running with the same slug returns the existing merchant (no duplicate row),
// so provisioning is safe to retry. Default-merchant seeding of catalog/credit
// definitions is left to the existing bootstrap/seed paths and is not duplicated
// here.
//
// The returned bool reports whether THIS call inserted the directory row
// (true) or the row already existed (false) — derived from the INSERT's own
// RETURNING, not a separate existence check, so it stays correct under
// concurrent first-provisions of the same slug: exactly one racing caller
// observes true (#750).
func (s *Service) Provision(ctx context.Context, req ProvisionRequest) (*Merchant, bool, error) {
	slug := normalizeSlug(req.Slug)
	if slug == "" {
		return nil, false, errors.New("merchants: provision requires a slug")
	}
	// #567: the merchant slug must be a legal AuthKit permission-group instance slug.
	if err := merchant.ValidateSlug(slug); err != nil {
		return nil, false, err
	}
	groupID := strings.TrimSpace(req.PermissionGroupID)
	if groupID == "" {
		return nil, false, ErrPermissionGroupRequired
	}

	// 1. Insert the merchant directory row. ON CONFLICT(slug) DO NOTHING keeps
	//    the existing row; RETURNING only yields a row when THIS statement
	//    performed the insert, which is how `created` is derived race-safely.
	var insertedID string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO openrails.merchants (slug, status, permission_group_id)
		VALUES ($1, 'active', NULLIF($2,''))
		ON CONFLICT (slug) DO NOTHING
		RETURNING id::text
	`, slug, groupID).Scan(&insertedID)
	created := true
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		created = false
	case err != nil:
		return nil, false, fmt.Errorf("merchants: insert merchant %q: %w", slug, err)
	}

	t, err := s.merchantBySlug(ctx, slug)
	if err != nil {
		return nil, false, err
	}

	// 2. Record the merchant's own permission-group id (#567). Idempotent.
	if _, uerr := s.pool.Exec(ctx, `
			UPDATE openrails.merchants
			   SET permission_group_id = $2, updated_at = current_timestamp
			 WHERE id = $1::uuid AND permission_group_id IS DISTINCT FROM $2
		`, t.ID.String(), groupID); uerr != nil {
		return nil, false, fmt.Errorf("merchants: record permission group on merchant: %w", uerr)
	}
	t.PermissionGroupID = groupID

	t, err = s.merchantBySlug(ctx, slug)
	if err != nil {
		return nil, false, err
	}
	return t, created, nil
}

// Get returns the merchant directory row by id.
func (s *Service) Get(ctx context.Context, id merchant.ID) (*Merchant, error) {
	return s.merchantByID(ctx, id)
}

// GetBySlug returns the merchant directory row by slug.
func (s *Service) GetBySlug(ctx context.Context, slug string) (*Merchant, error) {
	return s.merchantBySlug(ctx, normalizeSlug(slug))
}

// SetDisplayName sets the human-readable name for an active merchant. An empty
// name is a no-op so repair calls cannot clear an existing value by omission.
func (s *Service) SetDisplayName(ctx context.Context, id merchant.ID, displayName string) error {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE openrails.merchants
		   SET display_name = $2, updated_at = current_timestamp
		 WHERE id = $1::uuid AND status = 'active' AND deleted_at IS NULL
	`, id.String(), displayName)
	if err != nil {
		return fmt.Errorf("merchants: set display name: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrMerchantNotFound
	}
	return nil
}

// SearchMerchants returns active merchant directory rows whose slug matches the
// (case-insensitive) query substring. It is the cross-merchant search backing
// the platform-superadmin API (issue #226); the CALLER is responsible for
// auditing the search (it is a sensitive cross-merchant read). limit is clamped.
func (s *Service) SearchMerchants(ctx context.Context, q string, limit int) ([]Merchant, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, errors.New("merchants: search requires a non-empty query")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	pattern := "%" + strings.ToLower(q) + "%"
	rows, err := s.pool.Query(ctx, `SELECT `+merchantSelectCols+`
		FROM openrails.merchants
		WHERE deleted_at IS NULL
		  AND lower(slug) LIKE $1
		ORDER BY slug LIMIT $2`, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("merchants: search merchants: %w", err)
	}
	defer rows.Close()
	var out []Merchant
	for rows.Next() {
		t, serr := scanMerchant(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func normalizeSlug(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

const merchantSelectCols = `id::text, slug, status, COALESCE(permission_group_id,'')`

func scanMerchant(row pgx.Row) (*Merchant, error) {
	var (
		t      Merchant
		idStr  string
		status string
	)
	if err := row.Scan(&idStr, &t.Slug, &status, &t.PermissionGroupID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMerchantNotFound
		}
		return nil, err
	}
	id, err := merchant.ParseID(idStr)
	if err != nil {
		return nil, err
	}
	t.ID = id
	t.Status = MerchantStatus(status)
	return &t, nil
}

func (s *Service) merchantBySlug(ctx context.Context, slug string) (*Merchant, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+merchantSelectCols+`
		FROM openrails.merchants WHERE slug = $1 AND deleted_at IS NULL`, slug)
	return scanMerchant(row)
}

func (s *Service) merchantByID(ctx context.Context, id merchant.ID) (*Merchant, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+merchantSelectCols+`
		FROM openrails.merchants WHERE id = $1::uuid AND deleted_at IS NULL`, id.String())
	return scanMerchant(row)
}
