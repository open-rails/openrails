package merchants

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"

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
	// Slug is the current public name; PermissionGroupID owns billing identity.
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
var ErrMerchantBindingConflict = errors.New("merchants: name belongs to a different billing identity")
var ErrMerchantRetired = errors.New("merchants: billing identity retired")

// DestructivePolicy is the destructive-action gate a merchant purge must clear
// (#836 kill switch + #835 per-merchant policy). internal/destructive.Gate
// implements it; the indirection keeps this package free of that import and
// lets a nil gate mean "deny", not "skip".
type DestructivePolicy interface {
	// AllowDestructive reports whether destructive actions may execute for a
	// merchant, and why not when they may not. Implementations fail closed.
	AllowDestructive(ctx context.Context, merchantID uuid.UUID) (bool, string)
}

// deniedPolicy is the zero value: no gate wired means no purge. A merchant purge
// is unreachable by construction until an operator surface deliberately wires
// the real gate — which is the point (or#858: Service.Delete has no route and no
// CLI, and must not gain one while a purge is one-way).
type deniedPolicy struct{}

func (deniedPolicy) AllowDestructive(context.Context, uuid.UUID) (bool, string) {
	return false, "destructive gate not wired on the merchants service; refusing to purge (fail closed)"
}

// Service is the merchant provisioning + lifecycle service (issue #225). It owns
// the openrails.merchants directory rows (billing buckets) and per-merchant
// secrets. Control-plane callers create/resolve the AuthKit permission-group and
// pass its id explicitly; this service never creates AuthKit authority itself.
type Service struct {
	pool    *db.Pool
	secrets MerchantSecretStore
	// providerEnvironment is the deployment posture (#681): test under
	// test_mode, live otherwise. Scoped credential lookups resolve
	// psps rows in THIS environment only.
	providerEnvironment string
	// nmiProbeV5BaseURL is a test-only seam: overrides the base URL the #348
	// test_mode arm-time probe (refuseLiveNMIUnderTestMode) hits, so tests can
	// point it at a fake gateway instead of the real NMI API. Empty in
	// production — the probe uses nmi.NewClient's documented default.
	nmiProbeV5BaseURL string
	// Credential-probe endpoint overrides are test-only seams for the
	// read-only NMI Query API and CCBill DataLink checks.
	nmiCredentialProbeQueryURL   string
	ccbillCredentialProbeBaseURL string
	// destructive gates the merchant purge (or#858). Never nil: NewService seeds
	// it with deniedPolicy so an unwired service cannot purge.
	destructive DestructivePolicy
	// groupSlugResolver is the or#914 rename-forwarding seam: slug lookups
	// that miss the directory table retry through the authkit merchant-group
	// namespace (which follows ak#264 tombstones). Nil = table-only.
	groupSlugResolver GroupSlugResolver
}

// WithDestructivePolicy wires the destructive-action gate the merchant purge
// must clear. Without it, Delete refuses.
func (s *Service) WithDestructivePolicy(p DestructivePolicy) *Service {
	if s != nil && p != nil {
		s.destructive = p
	}
	return s
}

// NewService builds the lifecycle service. pool is required (it owns the merchant
// directory). secrets may be nil (credential management disabled).
// providerEnvironment is the deployment's PSP environment —
// derive it via config.ExpectedProviderEnvironment(cfg.IsTestMode()).
func NewService(pool *db.Pool, secrets MerchantSecretStore, providerEnvironment string) (*Service, error) {
	if pool == nil {
		return nil, errors.New("merchants: pgx pool is required")
	}
	env := normalizeProviderSecretEnvironment(providerEnvironment)
	if env == "" {
		return nil, fmt.Errorf("merchants: provider environment must be live or test, got %q", providerEnvironment)
	}
	return &Service{pool: pool, secrets: secrets, providerEnvironment: env, destructive: deniedPolicy{}}, nil
}

// NewDirectoryService builds a directory-only Service: merchant provisioning +
// lookup over openrails.merchants, with no secret store and no PSP
// environment (scoped credential lookups are unavailable). It is the lifecycle
// slice the control-plane provisioning seam needs (#738).
func NewDirectoryService(pool *db.Pool) (*Service, error) {
	if pool == nil {
		return nil, errors.New("merchants: pgx pool is required")
	}
	return &Service{pool: pool, destructive: deniedPolicy{}}, nil
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

// Provision binds billing identity to the immutable AuthKit group. A conflicting
// name can never authorize replacing an existing group's binding.
func (s *Service) Provision(ctx context.Context, req ProvisionRequest) (*Merchant, bool, error) {
	slug := normalizeSlug(req.Slug)
	if err := merchant.ValidateSlug(slug); err != nil {
		return nil, false, err
	}
	groupID := strings.TrimSpace(req.PermissionGroupID)
	if groupID == "" {
		return nil, false, ErrPermissionGroupRequired
	}

	// Repeated provisioning (including after rename) settles by group, not name.
	m, err := s.merchantByGroupID(ctx, groupID)
	if err == nil {
		return m, false, nil
	}
	if !errors.Is(err, ErrMerchantNotFound) {
		return nil, false, err
	}
	var insertedID string
	err = s.pool.QueryRow(ctx, `
  INSERT INTO openrails.merchants (slug, status, permission_group_id)
  VALUES ($1, 'active', $2)
  ON CONFLICT DO NOTHING
  RETURNING id::text
 `, slug, groupID).Scan(&insertedID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("merchants: provision %q: %w", slug, err)
	}
	created := err == nil
	m, err = s.merchantByGroupID(ctx, groupID)
	if errors.Is(err, ErrMerchantNotFound) {
		return nil, false, fmt.Errorf("%w: %q", ErrMerchantBindingConflict, slug)
	}
	return m, created, err
}

// merchantByGroupID includes retired rows so the same group cannot silently
// acquire a new billing identity after deletion.
func (s *Service) merchantByGroupID(ctx context.Context, groupID string) (*Merchant, error) {
	m, err := scanMerchant(s.pool.QueryRow(ctx, `SELECT `+merchantSelectCols+`
  FROM openrails.merchants WHERE permission_group_id = $1`, groupID))
	if err == nil && m.Status != StatusActive {
		return nil, ErrMerchantRetired
	}
	return m, err
}

// Get returns the merchant directory row by id.
func (s *Service) Get(ctx context.Context, id merchant.ID) (*Merchant, error) {
	return s.merchantByID(ctx, id)
}

// GetBySlug returns the merchant directory row by slug. A miss retries
// through the authkit group namespace when the or#914 rename-forwarding seam
// is wired (renamed-away slugs forward via their ak#264 tombstones).
func (s *Service) GetBySlug(ctx context.Context, slug string) (*Merchant, error) {
	norm := normalizeSlug(slug)
	m, err := s.merchantBySlug(ctx, norm)
	if errors.Is(err, ErrMerchantNotFound) {
		return s.merchantByGroupFallback(ctx, norm)
	}
	return m, err
}

// DirectoryRef is a merchant's public-facing directory identity: the slug it is
// addressed by and the human-readable name an operator gave it.
type DirectoryRef struct {
	Slug        string
	DisplayName string
}

// maxDirectoryRefSlugs bounds one directory-ref lookup. It exists so a host
// forwarding a caller-supplied slug list cannot turn this into an unbounded
// array scan; it errors rather than truncating, because a silently short answer
// would read as "those merchants do not exist".
const maxDirectoryRefSlugs = 200

// ListDirectoryRefs returns the directory identity of each requested slug, for
// the slugs that exist. It is the read counterpart of SetDisplayName: hosts that
// hold a merchant membership know only slugs and need names to label their own
// surfaces. Unknown slugs are omitted rather than erroring — the caller asked
// about a set, not a specific row. Slugs are normalized and deduped like every
// other directory lookup, and an empty request is not a query. Soft-deleted
// merchants never resolve; suspended ones still do, since a host that holds a
// membership must be able to label it whatever the merchant's status.
func (s *Service) ListDirectoryRefs(ctx context.Context, slugs []string) ([]DirectoryRef, error) {
	if len(slugs) > maxDirectoryRefSlugs {
		return nil, fmt.Errorf("merchants: list directory refs: %d slugs exceeds the %d limit", len(slugs), maxDirectoryRefSlugs)
	}
	normalized := make([]string, 0, len(slugs))
	seen := make(map[string]struct{}, len(slugs))
	for _, slug := range slugs {
		slug = normalizeSlug(slug)
		if slug == "" {
			continue
		}
		if _, dup := seen[slug]; dup {
			continue
		}
		seen[slug] = struct{}{}
		normalized = append(normalized, slug)
	}
	if len(normalized) == 0 {
		return nil, nil
	}
	rows, err := gen.New(s.pool).ListMerchantDirectoryRefs(ctx, normalized)
	if err != nil {
		return nil, fmt.Errorf("merchants: list directory refs: %w", err)
	}
	refs := make([]DirectoryRef, 0, len(rows))
	for _, row := range rows {
		refs = append(refs, DirectoryRef{Slug: row.Slug, DisplayName: row.DisplayName})
	}
	return refs, nil
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
