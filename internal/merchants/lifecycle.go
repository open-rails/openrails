package merchants

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"

	"github.com/open-rails/openrails/pkg/merchant"
)

// MerchantStatus mirrors billing.merchants.status.
type MerchantStatus string

const (
	StatusActive  MerchantStatus = "active"
	StatusDeleted MerchantStatus = "deleted"
)

// Merchant is the directory view of a row in billing.merchants.
type Merchant struct {
	ID                merchant.ID
	Slug              string
	Status            MerchantStatus
	PermissionGroupID string // The merchant's own AuthKit permission-group id (#567).
}

// ProvisionRequest parameterizes merchant provisioning.
type ProvisionRequest struct {
	// ID optionally preallocates the billing identity; zero generates one.
	ID merchant.ID
	// Slug is the name the new merchant claims.
	Slug string
	// PermissionGroupID is the merchant's own AuthKit permission-group id (#567).
	// Required for control-plane provisioning; embedded/no-AuthKit registration
	// uses internal/db.RegisterUnboundMerchant.
	PermissionGroupID string
}

// ErrMerchantNotFound indicates no billing.merchants row matched.
var ErrMerchantNotFound = errors.New("merchants: merchant not found")

// ErrPermissionGroupRequired indicates control-plane merchant provisioning
// tried to create a merchant namespace without its authkit permission-group id.
var ErrPermissionGroupRequired = errors.New("merchants: permission group required")
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
// the billing.merchants directory rows (billing buckets) and per-merchant
// secrets. Control-plane callers create/resolve the AuthKit permission-group and
// pass its id explicitly; this service never creates AuthKit authority itself.
type Service struct {
	StripeClients *stripeapi.Factory
	pool          *db.Pool
	database      *db.DB
	secrets       MerchantSecretStore
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
	// clock and webhookSecretOverlap bound rotated webhook secrets (SEC-29).
	clock                clockwork.Clock
	webhookSecretOverlap time.Duration
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
	service, err := NewDirectoryService(pool)
	if err != nil {
		return nil, err
	}
	service.secrets = secrets
	service.providerEnvironment = env
	return service, nil
}

// NewDirectoryService builds a directory-only Service: merchant provisioning +
// lookup over billing.merchants, with no secret store and no PSP
// environment (scoped credential lookups are unavailable). It is the lifecycle
// slice the control-plane provisioning seam needs (#738).
func NewDirectoryService(pool *db.Pool) (*Service, error) {
	if pool == nil {
		return nil, errors.New("merchants: pgx pool is required")
	}
	database, err := db.NewWithPGXPool(pool.Raw(), pool.Schema())
	if err != nil {
		return nil, err
	}
	return &Service{pool: pool, database: database, destructive: deniedPolicy{}}, nil
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

// Provision claims req.Slug for a new merchant bound to req.PermissionGroupID.
// bind runs first, in the same transaction, and creates that group, so a claim
// that fails leaves no group behind. A name held by another merchant is
// ErrMerchantNameTaken.
func (s *Service) Provision(ctx context.Context, req ProvisionRequest, bind func(context.Context, pgx.Tx) error) (*Merchant, error) {
	slug := normalizeSlug(req.Slug)
	if err := merchant.ValidateSlug(slug); err != nil {
		return nil, err
	}
	groupID := strings.TrimSpace(req.PermissionGroupID)
	if groupID == "" {
		return nil, ErrPermissionGroupRequired
	}
	id := req.ID.UUID()
	if req.ID.IsZero() {
		var err error
		if id, err = uuid.NewV7(); err != nil {
			return nil, err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if bind != nil {
		if err := bind(ctx, tx); err != nil {
			return nil, err
		}
	}
	if err := gen.New(tx).InsertMerchant(ctx, gen.InsertMerchantParams{ID: id, Slug: slug, GroupID: groupID}); err != nil {
		return nil, fmt.Errorf("merchants: provision %q: %w", slug, nameClaimError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.merchantByGroupID(ctx, groupID)
}

// merchantByGroupID includes retired rows so the same group cannot silently
// acquire a new billing identity after deletion.
func (s *Service) merchantByGroupID(ctx context.Context, groupID string) (*Merchant, error) {
	row, err := s.database.Gen(ctx).GetMerchantByGroupID(ctx, groupID)
	m, err := toMerchant(row.ID, row.Slug, row.Status, row.PermissionGroupID, err)
	if err == nil && m.Status != StatusActive {
		return nil, ErrMerchantRetired
	}
	return m, err
}

// Get returns the merchant directory row by id.
func (s *Service) Get(ctx context.Context, id merchant.ID) (*Merchant, error) {
	return s.merchantByID(ctx, id)
}

// GetByGroupID selects the billing identity already authorized by the caller.
func (s *Service) GetByGroupID(ctx context.Context, groupID string) (*Merchant, error) {
	if strings.TrimSpace(groupID) == "" {
		return nil, ErrPermissionGroupRequired
	}
	return s.merchantByGroupID(ctx, groupID)
}

// DirectoryRef is a merchant's public-facing directory identity: its name, the
// human-readable name an operator gave it, and its AuthKit group.
type DirectoryRef struct {
	ID          merchant.ID
	Slug        string
	DisplayName string
	GroupID     string
}

// SetDisplayName sets the human-readable name for an active merchant. An empty
// name is a no-op so repair calls cannot clear an existing value by omission.
func (s *Service) SetDisplayName(ctx context.Context, id merchant.ID, displayName string) error {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return nil
	}
	n, err := gen.New(s.pool).SetMerchantDisplayName(ctx, gen.SetMerchantDisplayNameParams{ID: id.UUID(), DisplayName: displayName})
	if err != nil {
		return fmt.Errorf("merchants: set display name: %w", err)
	}
	if n == 0 {
		return ErrMerchantNotFound
	}
	return nil
}

func normalizeSlug(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// toMerchant maps a directory row read with err; no row is ErrMerchantNotFound.
func toMerchant(id uuid.UUID, slug, status string, groupID *string, err error) (*Merchant, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrMerchantNotFound
	}
	if err != nil {
		return nil, err
	}
	m := &Merchant{ID: merchant.ID(id), Slug: slug, Status: MerchantStatus(status)}
	if groupID != nil {
		m.PermissionGroupID = *groupID
	}
	return m, nil
}

func (s *Service) merchantByID(ctx context.Context, id merchant.ID) (*Merchant, error) {
	row, err := s.database.Gen(ctx).GetMerchantDirectoryByID(ctx, id.UUID())
	return toMerchant(row.ID, row.Slug, row.Status, row.PermissionGroupID, err)
}
