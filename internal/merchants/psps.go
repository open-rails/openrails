package merchants

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

var (
	// ErrPSPNotFound: the merchant has no such PSP.
	ErrPSPNotFound = apperr.New(http.StatusNotFound, "psp_not_found", "merchants: PSP not found")
	// ErrPSPExists: the account is already a PSP, of this merchant or another.
	ErrPSPExists = apperr.New(http.StatusConflict, "psp_exists", "merchants: the account is already a PSP")
	// ErrPSPKeyTaken: another live PSP holds the key.
	ErrPSPKeyTaken = apperr.New(http.StatusConflict, "psp_key_taken", "merchants: another live PSP holds this key")
	// ErrPSPCredentialsRejected: the provider refused the credentials for this
	// deployment's posture.
	ErrPSPCredentialsRejected = apperr.New(http.StatusBadRequest, "psp_credentials_rejected", "the provider rejected the credentials")
	// ErrPSPClaimUnproven refuses a merchant's first claim of an account whose
	// credentials do not prove control of it (SEC-33). The operator declares
	// such accounts instead.
	ErrPSPClaimUnproven = apperr.New(http.StatusForbidden, "psp_claim_requires_proof", "provider account claims require credentials that prove control of the account, or operator declaration")
)

// providerCredentialError types a provider-side credential rejection; any
// other probe outcome (transport, indeterminate) stays an internal failure.
func providerCredentialError(err error) error {
	if errors.Is(err, nmi.ErrCredentialsRejected) || errors.Is(err, nmi.ErrLiveCredentialsUnderTestMode) || errors.Is(err, nmi.ErrTestModeUnderLivePosture) || errors.Is(err, nmi.ErrSandboxEndpointUnderLive) {
		return fmt.Errorf("%w: %v", ErrPSPCredentialsRejected, err)
	}
	return err
}

// LastActivePSPError refuses to archive the only active PSP on a rail without
// AllowLast.
type LastActivePSPError struct {
	PSP billing.PSPID
}

func (e *LastActivePSPError) Error() string {
	return fmt.Sprintf("merchants: PSP %s is the only active PSP on its rail; pass allow_last to archive it and refuse new checkout on the rail", e.PSP)
}

// pspPublication is one write of a PSP's settings and credentials.
type pspPublication struct {
	OperationID      uuid.UUID
	ExpectedRevision int64
	// Key names a PSP being created; an existing PSP keeps its key.
	Key                  string
	Settings             map[string]any
	Credentials          map[string]string
	RetireWebhookOverlap bool
}

var pspKeyShape = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// pspSettingKeys are the settings a PSP on rail takes through the API.
func pspSettingKeys(rail string) []string {
	switch rail {
	case string(models.RailStripe):
		return []string{"publishable_key"}
	case string(models.RailNMI):
		return []string{"tokenization_key"}
	}
	return []string{}
}

// RailDefinitions lists the rails a merchant can arm a PSP on, in registry
// order.
func RailDefinitions() []billing.RailDefinition {
	descriptors := rails.All()
	out := make([]billing.RailDefinition, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if !descriptor.HasPSPs {
			continue
		}
		credentialKeys := rails.MerchantCredentialKeyNames(descriptor.Rail)
		if credentialKeys == nil {
			credentialKeys = []string{}
		}
		out = append(out, billing.RailDefinition{
			Rail:           billing.Rail(descriptor.Rail),
			DisplayName:    descriptor.DisplayName,
			CredentialKeys: credentialKeys,
			SettingKeys:    pspSettingKeys(string(descriptor.Rail)),
		})
	}
	return out
}

// ListPSPs returns one page of the merchant's PSPs in the deployment's
// environment, newest first. Credential values are never returned.
func (s *Service) ListPSPs(ctx context.Context, id billing.MerchantID, params billing.PSPListParams) (billing.ListPage[billing.PSP], error) {
	if s == nil || s.pool == nil {
		return billing.ListPage[billing.PSP]{}, errors.New("merchants: pgx pool is required")
	}
	limit, err := pagination.Limit(params.PageRequest)
	if err != nil {
		return billing.ListPage[billing.PSP]{}, err
	}
	afterAt, afterID, err := pagination.After(params.Cursor)
	if err != nil {
		return billing.ListPage[billing.PSP]{}, err
	}
	var rail *string
	if params.Rail != "" {
		normalized := normalizeProviderSecretType(string(params.Rail))
		if !supportedRail(normalized) {
			return billing.ListPage[billing.PSP]{}, apperr.Invalidf("unknown rail %q", params.Rail).WithParam("rail")
		}
		rail = &normalized
	}
	var rows []gen.BillingPsp
	err = s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = gen.New(tx).ListPSPs(ctx, gen.ListPSPsParams{
			MerchantID: id.UUID(), Environment: s.providerEnvironment, Rail: rail, Archived: params.Archived,
			AfterAt: afterAt, AfterID: afterID, RowLimit: pagination.Fetch(limit),
		})
		return err
	})
	if err != nil {
		return billing.ListPage[billing.PSP]{}, err
	}
	page := pagination.Cut(rows, limit, func(row gen.BillingPsp) any {
		return pagination.TimeID{At: row.CreatedAt, ID: row.ID}
	})
	out := billing.ListPage[billing.PSP]{Items: make([]billing.PSP, 0, len(page.Items)), Next: page.Next}
	ids := make([]uuid.UUID, 0, len(page.Items))
	for _, row := range page.Items {
		ids = append(ids, row.ID)
	}
	obligations, err := s.pspOpenObligations(ctx, id, ids)
	if err != nil {
		return billing.ListPage[billing.PSP]{}, err
	}
	for _, row := range page.Items {
		psp, err := s.pspFromRow(ctx, id, row, obligations[row.ID])
		if err != nil {
			return billing.ListPage[billing.PSP]{}, err
		}
		out.Items = append(out.Items, psp)
	}
	return out, nil
}

// GetPSP reads one PSP.
func (s *Service) GetPSP(ctx context.Context, id billing.MerchantID, pspID billing.PSPID) (billing.PSP, error) {
	row, err := s.pspRow(ctx, id, pspID)
	if err != nil {
		return billing.PSP{}, err
	}
	return s.pspWithObligations(ctx, id, row)
}

func (s *Service) pspRow(ctx context.Context, id billing.MerchantID, pspID billing.PSPID) (gen.BillingPsp, error) {
	if s == nil || s.pool == nil {
		return gen.BillingPsp{}, errors.New("merchants: pgx pool is required")
	}
	if pspID.IsZero() {
		return gen.BillingPsp{}, ErrPSPNotFound
	}
	var row gen.BillingPsp
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = gen.New(tx).GetPSP(ctx, gen.GetPSPParams{MerchantID: id.UUID(), ID: pspID.UUID()})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.BillingPsp{}, ErrPSPNotFound
	}
	return row, err
}

// CreatePSP arms a new PSP: credentials are checked with the provider first,
// then stored. A merchant's first claim of an account must prove control of
// it through a successful credential check.
func (s *Service) CreatePSP(ctx context.Context, id billing.MerchantID, req billing.CreatePSPParams) (billing.PSP, error) {
	rail := normalizeProviderSecretType(string(req.Rail))
	if !supportedRail(rail) {
		return billing.PSP{}, apperr.Invalidf("unknown rail %q", req.Rail).WithParam("rail")
	}
	key := strings.ToLower(strings.TrimSpace(req.Key))
	if !pspKeyShape.MatchString(key) {
		return billing.PSP{}, apperr.Invalidf("key must be 1-63 lowercase letters, digits, - or _").WithParam("key")
	}
	accountID := strings.TrimSpace(req.AccountID)
	if accountID == "" {
		return billing.PSP{}, apperr.Invalidf("account_id is required").WithParam("account_id")
	}
	if err := config.ValidateRailAccountID(models.Rail(rail), accountID); err != nil {
		return billing.PSP{}, apperr.Invalidf("%v", err).WithParam("account_id")
	}
	return s.writePSP(ctx, id, rail, accountID, nil, pspPublication{
		OperationID: req.OperationID, Key: key, Settings: req.Settings, Credentials: req.Credentials,
	})
}

// UpdatePSP changes a PSP's settings or rotates its credentials. New
// credentials are checked with the provider before anything is stored; the
// old ones keep serving until the new ones are published.
func (s *Service) UpdatePSP(ctx context.Context, id billing.MerchantID, pspID billing.PSPID, req billing.UpdatePSPParams) (billing.PSP, error) {
	row, err := s.pspRow(ctx, id, pspID)
	if err != nil {
		return billing.PSP{}, err
	}
	return s.writePSP(ctx, id, row.Rail, row.AccountID, &row, pspPublication{
		OperationID: req.OperationID, ExpectedRevision: req.ExpectedRevision, Settings: req.Settings,
		Credentials: req.Credentials, RetireWebhookOverlap: req.RetireWebhookOverlap,
	})
}

// writePSP validates and publishes one PSP write. existing is nil for a
// create.
func (s *Service) writePSP(ctx context.Context, id billing.MerchantID, rail, accountID string, existing *gen.BillingPsp, req pspPublication) (billing.PSP, error) {
	if s == nil || s.pool == nil || s.secrets == nil {
		return billing.PSP{}, errors.New("merchants: PSP storage unavailable")
	}
	if req.OperationID == uuid.Nil {
		return billing.PSP{}, apperr.Invalidf("operation_id is required").WithParam("operation_id")
	}
	if req.ExpectedRevision < 0 {
		return billing.PSP{}, apperr.Invalidf("expected_revision must not be negative").WithParam("expected_revision")
	}
	if err := validatePSPSettings(rail, req.Settings, req.Credentials); err != nil {
		return billing.PSP{}, err
	}
	environment := s.providerEnvironment // derived from test_mode (#681/#882)
	if receipt, completed, err := s.replayProviderCredentialPublication(ctx, id, rail, environment, accountID, req); err != nil {
		return billing.PSP{}, err
	} else if completed {
		return s.pspWithObligations(ctx, id, receipt)
	}
	if len(req.Credentials) > 0 && !CanStageCredentials(s.secrets) {
		return billing.PSP{}, apperr.New(http.StatusMethodNotAllowed, "credential_source_read_only", "provider credential source has no writable durable custody")
	}
	if err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockLiveMerchantForSecretWrite(ctx, id.UUID()); err != nil {
			return err
		}
		if err := AssertPSPUnowned(ctx, q, id.UUID(), rail, environment, accountID); err != nil {
			return err
		}
		row, err := q.GetPSPByRailIdentity(ctx, gen.GetPSPByRailIdentityParams{MerchantID: id.UUID(), Rail: rail, Environment: &environment, AccountID: accountID})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if existing != nil {
				return ErrPSPNotFound
			}
			if _, err := q.GetActivePSPByKey(ctx, gen.GetActivePSPByKeyParams{MerchantID: id.UUID(), Key: req.Key, Environment: environment}); err == nil {
				return ErrPSPKeyTaken
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			return nil
		case err != nil:
			return err
		case existing == nil:
			return ErrPSPExists
		}
		if custody := credentialState(row).Custody; custody != "" && custody != SecretCustodyIdentity(s.secrets) {
			return ErrCredentialCustodyTransitionRequired
		}
		return nil
	}); err != nil {
		return billing.PSP{}, err
	}
	if err := s.refuseLiveNMIUnderTestMode(ctx, id, rail, environment, accountID, req.Credentials); err != nil {
		return billing.PSP{}, err
	}

	// secretNames maps the scoped secret NAME to its value; secretKeys maps the
	// same name back to the normalized credential KEY, which is what the
	// version floor on the PSP row is recorded under.
	secretNames := make(map[string]string, len(req.Credentials))
	secretKeys := make(map[string]string, len(req.Credentials))
	probeCredentials := make(map[string]string, len(req.Credentials))
	for key, value := range req.Credentials {
		normalizedKey, err := NormalizePSPSecretKey(rail, key)
		if err != nil {
			return billing.PSP{}, err
		}
		name, err := PSPSecretName(rail, environment, accountID, key)
		if err != nil {
			return billing.PSP{}, err
		}
		if rail == "stripe" && normalizedKey == "secret_key" {
			if err := validateSecretValueLocal(name, value); err != nil {
				return billing.PSP{}, err
			}
		} else if err := s.ValidateCredential(ctx, id, name, value, nil); err != nil {
			return billing.PSP{}, err
		}
		secretNames[name] = value
		secretKeys[name] = normalizedKey
		probeCredentials[normalizedKey] = value
	}
	// ROTATION ORDER (or#812). The live probe runs FIRST and on the credentials
	// SUPPLIED IN THIS REQUEST, so a bad new credential fails here — before any
	// secret is written and before any version floor moves. The old credential
	// stays exactly as it was and keeps serving on every node.
	probed, err := s.probePaymentProviderCredentials(ctx, id, rail, environment, accountID, probeCredentials)
	if err != nil {
		return billing.PSP{}, err
	}
	if existing == nil && !probed && claimNeedsProof(rail) {
		return billing.PSP{}, ErrPSPClaimUnproven
	}
	var validatedAt *time.Time
	if probed {
		now := time.Now().UTC()
		validatedAt = &now
	}
	row, err := s.publishProviderCredentials(ctx, id, rail, environment, accountID, req, secretNames, secretKeys, probed, validatedAt, webhookPublication{RetireWebhookOverlap: req.RetireWebhookOverlap})
	if err != nil {
		return billing.PSP{}, err
	}
	return s.pspWithObligations(ctx, id, row)
}

// claimNeedsProof lists rails whose inbound events route by account id.
func claimNeedsProof(rail string) bool {
	return rail == "stripe" || rail == "nmi" || rail == "ccbill"
}

// validatePSPSettings accepts the rail's API settings only, and never a
// credential stored as a setting.
func validatePSPSettings(rail string, settings map[string]any, credentials map[string]string) error {
	allowed := pspSettingKeys(rail)
	for key, raw := range settings {
		known := false
		for _, candidate := range allowed {
			known = known || candidate == key
		}
		if !known {
			return apperr.Invalidf("%s takes no setting %q", rail, key).WithParam("settings")
		}
		if raw == nil {
			continue
		}
		value, ok := raw.(string)
		if !ok {
			return apperr.Invalidf("setting %q must be a string", key).WithParam("settings")
		}
		if key == "publishable_key" && value != "" && !strings.HasPrefix(value, "pk_") {
			return apperr.Invalidf("publishable_key must be a Stripe pk_ key").WithParam("settings")
		}
		for _, secret := range credentials {
			if value != "" && value == secret {
				return apperr.Invalidf("a credential cannot be stored as a setting").WithParam("settings")
			}
		}
	}
	return nil
}

// mergeSettings overlays a write's settings on the stored ones; a null or
// empty value removes the key.
func mergeSettings(stored map[string]any, write map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(write))
	for key, value := range stored {
		out[key] = value
	}
	for key, value := range write {
		if text, ok := value.(string); value == nil || (ok && strings.TrimSpace(text) == "") {
			delete(out, key)
			continue
		}
		out[key] = value
	}
	return out
}

// mapPSPWriteError reads a unique violation of the live-key index as the
// key's refusal.
func mapPSPWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "psps_live_key_key" {
		return ErrPSPKeyTaken
	}
	return err
}

// ArchivePSP archives one PSP (#655/#656). It never contacts the provider — a
// terminated or dark account must still be archivable — and never touches the
// stored credentials, so existing obligations and inbound webhooks keep
// draining. Archiving an archived PSP returns it unchanged. The only active
// PSP on its rail is refused unless req.AllowLast.
func (s *Service) ArchivePSP(ctx context.Context, id billing.MerchantID, pspID billing.PSPID, req billing.ArchivePSPParams) (billing.PSP, error) {
	if s == nil || s.pool == nil {
		return billing.PSP{}, errors.New("merchants: PSP storage unavailable")
	}
	target, err := s.pspRow(ctx, id, pspID)
	if err != nil {
		return billing.PSP{}, err
	}
	var out gen.BillingPsp
	err = s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		// One lock order over the rail's rows, so two concurrent archives
		// cannot each see the other as the remaining active PSP.
		rows, err := q.LockPSPsForRailEnvironment(ctx, gen.LockPSPsForRailEnvironmentParams{MerchantID: id.UUID(), Rail: target.Rail, Environment: target.Environment})
		if err != nil {
			return err
		}
		var current *gen.BillingPsp
		otherActive := 0
		for i := range rows {
			switch {
			case rows[i].ID == target.ID:
				current = &rows[i]
			case !rows[i].Archived:
				otherActive++
			}
		}
		if current == nil {
			return ErrPSPNotFound
		}
		if current.Archived {
			out = *current
			return nil
		}
		if otherActive == 0 && !req.AllowLast {
			return &LastActivePSPError{PSP: billing.PSPID(current.ID)}
		}
		out, err = q.ArchivePSP(ctx, gen.ArchivePSPParams{ID: current.ID, MerchantID: id.UUID()})
		return err
	})
	if err != nil {
		return billing.PSP{}, err
	}
	return s.pspWithObligations(ctx, id, out)
}

func (s *Service) pspWithObligations(ctx context.Context, id billing.MerchantID, row gen.BillingPsp) (billing.PSP, error) {
	obligations, err := s.pspOpenObligations(ctx, id, []uuid.UUID{row.ID})
	if err != nil {
		return billing.PSP{}, err
	}
	return s.pspFromRow(ctx, id, row, obligations[row.ID])
}

func (s *Service) pspOpenObligations(ctx context.Context, id billing.MerchantID, pspIDs []uuid.UUID) (map[uuid.UUID]int64, error) {
	out := make(map[uuid.UUID]int64, len(pspIDs))
	if len(pspIDs) == 0 {
		return out, nil
	}
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := gen.New(tx).CountPSPOpenObligations(ctx, gen.CountPSPOpenObligationsParams{MerchantID: id.UUID(), PspIds: pspIDs})
		for _, row := range rows {
			out[row.PspID] = row.OpenObligations
		}
		return err
	})
	return out, err
}

func (s *Service) pspFromRow(ctx context.Context, id billing.MerchantID, row gen.BillingPsp, openObligations int64) (billing.PSP, error) {
	statuses, err := s.pspCredentialStatuses(ctx, id, row)
	if err != nil {
		return billing.PSP{}, err
	}
	return pspView(row, statuses, openObligations), nil
}

// pspView is the API view of a PSP row: credential slots by state, never by
// value.
func pspView(row gen.BillingPsp, statuses []MerchantSecretStatus, openObligations int64) billing.PSP {
	state := credentialState(row)
	configured := map[string]struct{}{}
	for _, status := range statuses {
		if status.Configured {
			configured[status.Key] = struct{}{}
		}
	}
	validatedAt := state.ValidatedAt
	if !pspValidationCredentialsConfigured(row.Rail, configured) {
		validatedAt = nil
	}
	credentials := make(map[string]billing.PSPCredential)
	for _, key := range pspCredentialKeys(row.Rail) {
		_, ok := configured[key]
		credential := billing.PSPCredential{RotationVersion: state.Versions[NormalizeCredentialVersionKey(key)], Configured: ok}
		if ok {
			credential.ValidatedAt = credentialValidatedAt(row.Rail, key, validatedAt)
		}
		credentials[key] = credential
	}
	settings := rowSettings(row)
	if settings == nil {
		settings = map[string]any{}
	}
	return billing.PSP{
		ID:              billing.PSPID(row.ID),
		Key:             row.Key,
		Rail:            billing.Rail(row.Rail),
		Environment:     row.Environment,
		AccountID:       row.AccountID,
		Archived:        row.Archived,
		ArchivedAt:      row.ArchivedAt,
		OpenObligations: openObligations,
		Settings:        settings,
		Credentials:     credentials,
		Revision:        row.Revision,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

// pspValidationCredentialsConfigured reports whether the credentials the
// provider check reads are all present.
func pspValidationCredentialsConfigured(rail string, configured map[string]struct{}) bool {
	var required []string
	switch rail {
	case "stripe":
		required = []string{"secret_key"}
	case "nmi":
		required = []string{"security_key"}
	case "ccbill":
		required = []string{"datalink_username", "datalink_password"}
	default:
		return false
	}
	for _, key := range required {
		if _, ok := configured[key]; !ok {
			return false
		}
	}
	return true
}

// supportedRail is registry-backed (#669): the rail takes PSPs.
func supportedRail(rail string) bool {
	return rails.SupportsPSPs(models.Rail(rail))
}

// pspCredentialKeys returns the MERCHANT-visible credential slots
// (registry-backed, #669). Operator-only secrets (solana private_key) are
// deliberately absent from the merchant credential view.
func pspCredentialKeys(rail string) []string {
	return rails.MerchantCredentialKeyNames(models.Rail(rail))
}

func credentialValidatedAt(rail, key string, validatedAt *time.Time) *time.Time {
	if validatedAt == nil {
		return nil
	}
	switch rail {
	case "stripe":
		if key == "secret_key" {
			return validatedAt
		}
	case "nmi":
		if key == "security_key" {
			return validatedAt
		}
	case "ccbill":
		if key == "datalink_username" || key == "datalink_password" {
			return validatedAt
		}
	}
	return nil
}

// mergeCredentialVersions carries forward the version floors of credentials
// this request did not touch and raises the floors it did. A floor NEVER goes
// backwards: a lower observed version means an out-of-order write, and the
// higher floor is the safe one (it only ever forces a re-read).
func mergeCredentialVersions(existing, rotated map[string]int) map[string]int {
	if len(existing) == 0 && len(rotated) == 0 {
		return nil
	}
	out := make(map[string]int, len(existing)+len(rotated))
	for k, v := range existing {
		if k = NormalizeCredentialVersionKey(k); k != "" && v > 0 {
			out[k] = v
		}
	}
	for k, v := range rotated {
		if k = NormalizeCredentialVersionKey(k); k != "" && v > out[k] {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// refuseLiveNMIUnderTestMode requires a fresh simulated result before arming
// sandbox NMI credentials. Live or indeterminate responses refuse the arm.
func (s *Service) refuseLiveNMIUnderTestMode(ctx context.Context, id billing.MerchantID, rail, environment, accountID string, credentials map[string]string) error {
	if rail != string(models.RailNMI) {
		return nil
	}
	sandbox := s.providerEnvironment == "test"
	name, err := PSPSecretName(rail, environment, accountID, "security_key")
	if err != nil {
		return err
	}
	securityKey := strings.TrimSpace(credentials["security_key"])
	if securityKey == "" && s.secrets != nil {
		// The write may only be touching other fields (settings, a second
		// credential) — resolve the EFFECTIVE key already on file so a live
		// account can't slip through by omitting security_key from this
		// particular request.
		if sec, gerr := s.readPublishedProviderCredential(ctx, id, rail, environment, accountID, "security_key", name); gerr == nil {
			securityKey = strings.TrimSpace(sec.Value)
		} else if !errors.Is(gerr, ErrSecretNotFound) {
			return fmt.Errorf("read effective NMI credential for sandbox qualification: %w", gerr)
		}
	}
	if securityKey == "" {
		return nil // unconfigured; nothing to verify
	}
	deployment, pspID, err := s.storedNMIDeployment(ctx, id, rail, environment, accountID)
	if err != nil {
		return err
	}
	client, err := nmi.NewAccountClient(id.UUID(), pspID, accountID, &config.NMIProviderSettings{SecurityKey: securityKey, EndpointDeployment: deployment}, sandbox)
	if err != nil {
		return fmt.Errorf("construct NMI posture qualification client: %w", err)
	}
	if s.nmiProbeV5BaseURL != "" {
		client.V5BaseURL = s.nmiProbeV5BaseURL
	}
	check := nmi.CheckTestModeArm
	if !sandbox {
		// SEC-33: a live deployment refuses an NMI account left in test mode.
		check = nmi.CheckLiveArm
	}
	if err := check(ctx, client); err != nil {
		return providerCredentialError(fmt.Errorf("merchants: rail %q account %q: %w", rail, accountID, err))
	}
	return nil
}

// storedNMIDeployment reads the declared endpoint deployment and PSP id; an
// undeclared PSP uses the default deployment and its derived natural-key id.
func (s *Service) storedNMIDeployment(ctx context.Context, id billing.MerchantID, rail, environment, accountID string) (string, uuid.UUID, error) {
	pspID, _, _, _ := PSPNaturalKey(rail, environment, accountID)
	if s.pool == nil {
		return "", pspID, nil
	}
	row, err := gen.New(s.pool).GetPSPByRailIdentity(ctx, gen.GetPSPByRailIdentityParams{MerchantID: id.UUID(), Rail: rail, Environment: &environment, AccountID: accountID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", uuid.Nil, err
	}
	var settings map[string]any
	if err == nil {
		pspID = row.ID
		settings = rowSettings(row)
	}
	deployment, err := config.NMIEndpointDeployment(settings)
	return deployment, pspID, err
}

// pspCredentialStatuses reads only the registry-bounded slots of the PSP.
// Published references select exact custody; fallback names are used only
// for unpublished slots.
func (s *Service) pspCredentialStatuses(ctx context.Context, id billing.MerchantID, row gen.BillingPsp) ([]MerchantSecretStatus, error) {
	var statuses []MerchantSecretStatus
	for _, key := range pspCredentialKeys(row.Rail) {
		ref, err := PSPSecretRef(row, key)
		if err != nil {
			return nil, err
		}
		if ref.Retired {
			continue
		}
		value, err := ReadSecretRef(ctx, s.secrets, id, ref)
		if err != nil && !errors.Is(err, ErrSecretNotFound) {
			return nil, err
		}
		statuses = append(statuses, MerchantSecretStatus{Name: ref.Name, Key: key, Rail: row.Rail, Configured: err == nil, Version: value.Version})
	}
	return statuses, nil
}

// DeclarePSP records a PSP identity without credentials, for imported billing
// facts attributed to it. It never arms the PSP for checkout; an account
// already declared keeps its id, key, archive state, custody and credentials.
// The environment is the deployment's.
func (s *Service) DeclarePSP(ctx context.Context, id billing.MerchantID, declaration billing.PSPDeclaration) (billing.PSP, error) {
	if s == nil || s.pool == nil {
		return billing.PSP{}, errors.New("merchants: PSP storage unavailable")
	}
	key := strings.ToLower(strings.TrimSpace(declaration.Key))
	rail := normalizeProviderSecretType(string(declaration.Rail))
	accountID := strings.TrimSpace(declaration.AccountID)
	switch {
	case id.IsZero():
		return billing.PSP{}, apperr.Invalidf("a merchant is required")
	case !pspKeyShape.MatchString(key):
		return billing.PSP{}, apperr.Invalidf("key must be 1-63 lowercase letters, digits, - or _").WithParam("key")
	case !supportedRail(rail):
		return billing.PSP{}, apperr.Invalidf("unknown rail %q", declaration.Rail).WithParam("rail")
	case accountID == "":
		return billing.PSP{}, apperr.Invalidf("account_id is required").WithParam("account_id")
	}
	environment := s.providerEnvironment
	if err := AssertPSPUnowned(ctx, gen.New(s.pool), id.UUID(), rail, environment, accountID); err != nil {
		return billing.PSP{}, err
	}
	pspID, nRail, nEnv, nAccount := PSPNaturalKey(rail, environment, accountID)
	var row gen.BillingPsp
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = gen.New(tx).DeclarePSPIdentity(ctx, gen.DeclarePSPIdentityParams{
			ID: pspID, MerchantID: id.UUID(), Rail: nRail, Environment: nEnv, AccountID: nAccount, Key: key,
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The account is declared under another key.
		return billing.PSP{}, ErrPSPExists
	}
	if err != nil {
		return billing.PSP{}, mapPSPWriteError(err)
	}
	return s.pspWithObligations(ctx, id, row)
}
