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
	ID         merchant.ID
	Slug       string
	Status     MerchantStatus
	OwnerOrgID string // AuthKit org that owns/administers this merchant namespace (#500).
}

// ProvisionRequest parameterizes merchant provisioning.
type ProvisionRequest struct {
	// Slug is the stable merchant slug (used in routes and merchant resolution).
	Slug string `json:"slug"`
	// OwnerOrgID is the AuthKit org uuid that owns/administers this merchant
	// namespace (#500 ownership link, NOT identity). Required for control-plane
	// provisioning; embedded/no-AuthKit registration uses internal/db.RegisterMerchant.
	OwnerOrgID string `json:"owner_org_id"`
}

// ErrMerchantNotFound indicates no openrails.merchants row matched.
var ErrMerchantNotFound = errors.New("merchants: merchant not found")

// ErrOwnerOrgRequired indicates control-plane merchant provisioning tried to
// create a merchant namespace without its durable AuthKit org owner.
var ErrOwnerOrgRequired = errors.New("merchants: owner org required")

// ErrExportRequired is returned by Delete when no completed export exists for the
// merchant (export-before-delete is enforced).
var ErrExportRequired = errors.New("merchants: export required before delete")

// Service is the merchant provisioning + lifecycle service (issue #225). It owns
// the openrails.merchants directory rows (billing buckets) and per-merchant
// secrets. #481: it no longer auto-mints an AuthKit org per merchant — owner-org
// ownership is set explicitly (the explicit register -> create-org ->
// create-remote_application -> create-merchant flow), never auto-minted here.
type Service struct {
	pool    *db.Pool
	secrets MerchantSecretStore
}

// NewService builds the lifecycle service. pool is required (it owns the merchant
// directory). secrets may be nil (credential management disabled).
func NewService(pool *db.Pool, secrets MerchantSecretStore) (*Service, error) {
	if pool == nil {
		return nil, errors.New("merchants: pgx pool is required")
	}
	return &Service{pool: pool, secrets: secrets}, nil
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
//  2. record the owning AuthKit org id supplied by the caller.
//
// Re-running with the same slug returns the existing merchant (no duplicate row),
// so provisioning is safe to retry. Default-merchant seeding of catalog/credit
// definitions is left to the existing bootstrap/seed paths and is not duplicated
// here.
func (s *Service) Provision(ctx context.Context, req ProvisionRequest) (*Merchant, error) {
	slug := normalizeSlug(req.Slug)
	if slug == "" {
		return nil, errors.New("merchants: provision requires a slug")
	}
	// #548: the merchant slug must be a legal AuthKit org slug (it owns a same-slug
	// backing org).
	if err := merchant.ValidateSlug(slug); err != nil {
		return nil, err
	}
	ownerOrgID := strings.TrimSpace(req.OwnerOrgID)
	if ownerOrgID == "" {
		return nil, ErrOwnerOrgRequired
	}

	// 1. Upsert the merchant directory row. ON CONFLICT(slug) DO NOTHING keeps the
	//    existing row, then we re-read so provision is idempotent. The AuthKit org
	//    owner is resolved/created by the caller and recorded here.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO openrails.merchants (slug, status, owner_org_id)
		VALUES ($1, 'active', NULLIF($2,''))
		ON CONFLICT (slug) DO NOTHING
	`, slug, ownerOrgID)
	if err != nil {
		return nil, fmt.Errorf("merchants: insert merchant %q: %w", slug, err)
	}

	t, err := s.merchantBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}

	// 2. Record the explicit owner-org ownership link (#500): the AuthKit org
	//    that administers this merchant namespace. Idempotent.
	if _, uerr := s.pool.Exec(ctx, `
			UPDATE openrails.merchants
			   SET owner_org_id = $2, updated_at = current_timestamp
			 WHERE id = $1::uuid AND owner_org_id IS DISTINCT FROM $2
		`, t.ID.String(), ownerOrgID); uerr != nil {
		return nil, fmt.Errorf("merchants: record owner org on merchant: %w", uerr)
	}
	t.OwnerOrgID = ownerOrgID

	return s.merchantBySlug(ctx, slug)
}

// Get returns the merchant directory row by id.
func (s *Service) Get(ctx context.Context, id merchant.ID) (*Merchant, error) {
	return s.merchantByID(ctx, id)
}

// GetBySlug returns the merchant directory row by slug.
func (s *Service) GetBySlug(ctx context.Context, slug string) (*Merchant, error) {
	return s.merchantBySlug(ctx, normalizeSlug(slug))
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

const merchantSelectCols = `id::text, slug, status, COALESCE(owner_org_id,'')`

func scanMerchant(row pgx.Row) (*Merchant, error) {
	var (
		t      Merchant
		idStr  string
		status string
	)
	if err := row.Scan(&idStr, &t.Slug, &status, &t.OwnerOrgID); err != nil {
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
