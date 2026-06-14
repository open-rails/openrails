package tenancy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"

	"github.com/open-rails/openrails/pkg/merchant"
)

// TenantStatus mirrors openrails.merchants.status.
type TenantStatus string

const (
	StatusActive    TenantStatus = "active"
	StatusSuspended TenantStatus = "suspended"
	StatusDeleted   TenantStatus = "deleted"
)

// Tenant is the directory view of a row in openrails.merchants.
type Tenant struct {
	ID            merchant.ID
	Slug          string
	Name          string
	Status        TenantStatus
	OwnerTenantID string // AuthKit org that owns/administers this merchant (#481); "" in embedded
	BillingTier   string
	Region        string
	WebhookHost   string
	WebhookPath   string
	SuspendedAt   *time.Time
}

// ProvisionRequest parameterizes tenant provisioning.
type ProvisionRequest struct {
	// Slug is the stable tenant slug (used in routes and tenant resolution).
	Slug string
	// Name is the human-readable merchant name.
	Name string
	// OwnerTenantID is the AuthKit org (tenant) uuid that owns/administers this
	// merchant (#481 ownership link, NOT identity). Optional; empty = unowned
	// (embedded, or owner linked later). Never auto-minted.
	OwnerTenantID string
	// BillingTier is the platform's own billing tier for this tenant (dogfood).
	BillingTier string
	// Region is optional hosting metadata.
	Region string
	// WebhookHost / WebhookPath register ingress webhook routing for this tenant.
	WebhookHost string
	WebhookPath string
}

// ErrTenantNotFound indicates no openrails.merchants row matched.
var ErrTenantNotFound = errors.New("tenancy: tenant not found")

// ErrExportRequired is returned by Delete when no completed export exists for the
// tenant (export-before-delete is enforced).
var ErrExportRequired = errors.New("tenancy: export required before delete")

// Service is the merchant provisioning + lifecycle service (issue #225). It owns
// the openrails.merchants directory rows (billing buckets) and per-merchant
// secrets. #481: it no longer auto-mints an AuthKit org per merchant — owner-tenant
// ownership is set explicitly (the explicit register -> create-tenant ->
// create-remote_application -> create-merchant flow), never auto-minted here.
type Service struct {
	pool    *db.Pool
	secrets TenantSecretStore
}

// NewService builds the lifecycle service. pool is required (it owns the merchant
// directory). secrets may be nil (credential management disabled).
func NewService(pool *db.Pool, secrets TenantSecretStore) (*Service, error) {
	if pool == nil {
		return nil, errors.New("tenancy: pgx pool is required")
	}
	return &Service{pool: pool, secrets: secrets}, nil
}

// NewSecretManagementService builds a secret-management-only Service. It is for
// runtimes/tests that only need credential list/write/delete/validate behavior;
// lifecycle methods such as Provision still require NewService with a DB pool.
func NewSecretManagementService(secrets TenantSecretStore) (*Service, error) {
	if secrets == nil {
		return nil, errors.New("tenancy: secret store is required")
	}
	return &Service{secrets: secrets}, nil
}

// Secrets exposes the per-tenant secret store (may be nil).
func (s *Service) Secrets() TenantSecretStore { return s.secrets }

// Provision idempotently provisions a tenant (issue #225):
//
//  1. create/ensure the openrails.merchants namespace row (resolve by slug),
//  2. create/link the operator AuthKit tenant via the control plane and record it,
//  3. record routing (webhook host/path) + billing tier + region.
//
// Re-running with the same slug returns the existing tenant (no duplicate row,
// no duplicate AuthKit tenant), so provisioning is safe to retry. Default-tenant seeding of
// catalog/credit definitions is left to the existing bootstrap/seed paths and is
// not duplicated here.
func (s *Service) Provision(ctx context.Context, req ProvisionRequest) (*Tenant, error) {
	slug := normalizeSlug(req.Slug)
	if slug == "" {
		return nil, errors.New("tenancy: provision requires a slug")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = slug
	}

	// 1. Upsert the merchant directory row. ON CONFLICT(slug) DO NOTHING keeps the
	//    existing row, then we re-read so provision is idempotent. #481: no AuthKit
	//    org is auto-minted — the owner-tenant link is set explicitly elsewhere.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO openrails.merchants (slug, name, status, owner_tenant_id, billing_tier, region, webhook_host, webhook_path, provisioned_at)
		VALUES ($1, $2, 'active', NULLIF($3,''), NULLIF($4,''), NULLIF($5,''), NULLIF($6,''), NULLIF($7,''), current_timestamp)
		ON CONFLICT (slug) DO NOTHING
	`, slug, name, strings.TrimSpace(req.OwnerTenantID), strings.TrimSpace(req.BillingTier), strings.TrimSpace(req.Region),
		strings.TrimSpace(req.WebhookHost), strings.TrimSpace(req.WebhookPath))
	if err != nil {
		return nil, fmt.Errorf("tenancy: insert merchant %q: %w", slug, err)
	}

	t, err := s.tenantBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}

	// 2. Record the explicit owner-tenant ownership link when supplied (#481): the
	//    AuthKit org that administers this merchant. Idempotent; never auto-minted.
	if owner := strings.TrimSpace(req.OwnerTenantID); owner != "" {
		if _, uerr := s.pool.Exec(ctx, `
			UPDATE openrails.merchants
			   SET owner_tenant_id = $2, updated_at = current_timestamp
			 WHERE id = $1::uuid AND owner_tenant_id IS DISTINCT FROM $2
		`, t.ID.String(), owner); uerr != nil {
			return nil, fmt.Errorf("tenancy: record owner tenant on merchant: %w", uerr)
		}
		t.OwnerTenantID = owner
	}

	// 3. Reconcile routing / tier / region for an already-existing row (provision
	//    is idempotent AND patches routing on re-run).
	if _, err := s.pool.Exec(ctx, `
		UPDATE openrails.merchants
		   SET billing_tier  = COALESCE(NULLIF($2,''), billing_tier),
		       region        = COALESCE(NULLIF($3,''), region),
		       webhook_host  = COALESCE(NULLIF($4,''), webhook_host),
		       webhook_path  = COALESCE(NULLIF($5,''), webhook_path),
		       updated_at    = current_timestamp
		 WHERE id = $1::uuid
	`, t.ID.String(), strings.TrimSpace(req.BillingTier), strings.TrimSpace(req.Region),
		strings.TrimSpace(req.WebhookHost), strings.TrimSpace(req.WebhookPath)); err != nil {
		return nil, fmt.Errorf("tenancy: reconcile tenant routing: %w", err)
	}

	return s.tenantBySlug(ctx, slug)
}

// Suspend marks a tenant suspended: reads return maintenance and writes are
// denied (enforced by IsWritable / the suspension middleware). Idempotent.
func (s *Service) Suspend(ctx context.Context, id merchant.ID) error {
	return s.setStatus(ctx, id, StatusSuspended, true)
}

// Resume clears suspension and returns the tenant to active. Idempotent.
func (s *Service) Resume(ctx context.Context, id merchant.ID) error {
	return s.setStatus(ctx, id, StatusActive, false)
}

// TierChange upgrades/downgrades the platform's own billing tier for this tenant
// (dogfood). Idempotent.
func (s *Service) TierChange(ctx context.Context, id merchant.ID, tier string) error {
	tier = strings.TrimSpace(tier)
	if tier == "" {
		return errors.New("tenancy: tier change requires a tier")
	}
	ct, err := s.pool.Exec(ctx, `
		UPDATE openrails.merchants SET billing_tier = $2, updated_at = current_timestamp
		 WHERE id = $1::uuid AND deleted_at IS NULL
	`, id.String(), tier)
	if err != nil {
		return fmt.Errorf("tenancy: tier change: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrTenantNotFound
	}
	return nil
}

func (s *Service) setStatus(ctx context.Context, id merchant.ID, status TenantStatus, suspended bool) error {
	var suspendedExpr string
	if suspended {
		suspendedExpr = "COALESCE(suspended_at, current_timestamp)"
	} else {
		suspendedExpr = "NULL"
	}
	ct, err := s.pool.Exec(ctx, fmt.Sprintf(`
		UPDATE openrails.merchants
		   SET status = $2, suspended_at = %s, updated_at = current_timestamp
		 WHERE id = $1::uuid AND deleted_at IS NULL
	`, suspendedExpr), id.String(), string(status))
	if err != nil {
		return fmt.Errorf("tenancy: set status %s: %w", status, err)
	}
	if ct.RowsAffected() == 0 {
		return ErrTenantNotFound
	}
	return nil
}

// IsWritable reports whether the tenant currently accepts writes. Suspended or
// deleted tenants are read-only; the default tenant and active tenants are
// writable. A missing tenant is treated as not writable.
func (s *Service) IsWritable(ctx context.Context, id merchant.ID) (bool, error) {
	t, err := s.tenantByID(ctx, id)
	if err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			return false, nil
		}
		return false, err
	}
	return t.Status == StatusActive, nil
}

// Get returns the tenant directory row by id.
func (s *Service) Get(ctx context.Context, id merchant.ID) (*Tenant, error) {
	return s.tenantByID(ctx, id)
}

// GetBySlug returns the tenant directory row by slug.
func (s *Service) GetBySlug(ctx context.Context, slug string) (*Tenant, error) {
	return s.tenantBySlug(ctx, normalizeSlug(slug))
}

// SearchTenants returns active tenant directory rows whose slug or name matches
// the (case-insensitive) query substring. It is the cross-tenant search backing
// the platform-superadmin API (issue #226); the CALLER is responsible for
// auditing the search (it is a sensitive cross-tenant read). limit is clamped.
func (s *Service) SearchTenants(ctx context.Context, q string, limit int) ([]Tenant, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, errors.New("tenancy: search requires a non-empty query")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	pattern := "%" + strings.ToLower(q) + "%"
	rows, err := s.pool.Query(ctx, `SELECT `+tenantSelectCols+`
		FROM openrails.merchants
		WHERE deleted_at IS NULL
		  AND (lower(slug) LIKE $1 OR lower(name) LIKE $1)
		ORDER BY slug LIMIT $2`, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("tenancy: search tenants: %w", err)
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		t, serr := scanTenant(rows)
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

const tenantSelectCols = `id::text, slug, name, status,
	COALESCE(owner_tenant_id,''),
	COALESCE(billing_tier,''), COALESCE(region,''),
	COALESCE(webhook_host,''), COALESCE(webhook_path,''), suspended_at`

func scanTenant(row pgx.Row) (*Tenant, error) {
	var (
		t           Tenant
		idStr       string
		status      string
		suspendedAt *time.Time
	)
	if err := row.Scan(&idStr, &t.Slug, &t.Name, &status,
		&t.OwnerTenantID, &t.BillingTier, &t.Region,
		&t.WebhookHost, &t.WebhookPath, &suspendedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrTenantNotFound
		}
		return nil, err
	}
	id, err := merchant.ParseID(idStr)
	if err != nil {
		return nil, err
	}
	t.ID = id
	t.Status = TenantStatus(status)
	t.SuspendedAt = suspendedAt
	return &t, nil
}

func (s *Service) tenantBySlug(ctx context.Context, slug string) (*Tenant, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+tenantSelectCols+`
		FROM openrails.merchants WHERE slug = $1 AND deleted_at IS NULL`, slug)
	return scanTenant(row)
}

func (s *Service) tenantByID(ctx context.Context, id merchant.ID) (*Tenant, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+tenantSelectCols+`
		FROM openrails.merchants WHERE id = $1::uuid AND deleted_at IS NULL`, id.String())
	return scanTenant(row)
}
