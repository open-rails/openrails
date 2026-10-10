package merchants

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
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
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

var (
	// ErrPSPNotFound: the merchant has no such PSP.
	ErrPSPNotFound = apperr.New(http.StatusNotFound, "psp_not_found", "merchants: PSP not found")
	// ErrPSPExists: the account is already a PSP, of this merchant or another.
	ErrPSPExists = apperr.New(http.StatusConflict, "psp_exists", "merchants: the account is already a PSP")
	// ErrPSPKeyTaken: another PSP holds the key.
	ErrPSPKeyTaken = apperr.New(http.StatusConflict, "psp_key_taken", "merchants: another PSP holds this key")
	// ErrPSPCredentialsRejected: the provider refused the credentials for this
	// deployment's posture.
	ErrPSPCredentialsRejected = apperr.New(http.StatusBadRequest, "psp_credentials_rejected", "the provider rejected the credentials")
	// ErrPSPClaimUnproven refuses a merchant's first claim of an account whose
	// credentials do not prove control of it (SEC-33). The operator declares
	// such accounts instead.
	ErrPSPClaimUnproven = apperr.New(http.StatusForbidden, "psp_claim_requires_proof", "provider account claims require credentials that prove control of the account, or operator declaration")
	// ErrRevisionMismatch refuses an edit naming a revision the document has
	// moved past.
	ErrRevisionMismatch = apperr.New(http.StatusConflict, billing.CodeRevisionMismatch, "the configuration changed since the revision the request names; read it again")
	// ErrConfigReadOnly refuses an edit of configuration read from a file.
	ErrConfigReadOnly = apperr.New(http.StatusConflict, "merchant_config_read_only", "the merchant's configuration is read from a file; change the file")
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
	j, err := s.load(ctx, id)
	if err != nil {
		return billing.ListPage[billing.PSP]{}, err
	}
	var selected []PSPScope
	if params.IDs != nil {
		for _, want := range params.IDs {
			if p, ok := j.byID(want.UUID()); ok && !slices.ContainsFunc(selected, func(q PSPScope) bool { return q.ID == p.ID }) {
				selected = append(selected, p)
			}
		}
		slices.SortFunc(selected, newestFirst)
		return s.pspPage(ctx, id, billing.ListPage[PSPScope]{Items: selected})
	}
	limit, err := pagination.Limit(params.PageRequest)
	if err != nil {
		return billing.ListPage[billing.PSP]{}, err
	}
	afterAt, afterID, err := pagination.After(params.Cursor)
	if err != nil {
		return billing.ListPage[billing.PSP]{}, err
	}
	rail := ""
	if params.Rail != "" {
		rail = normalizeProviderSecretType(string(params.Rail))
		if !supportedRail(rail) {
			return billing.ListPage[billing.PSP]{}, apperr.Invalidf("unknown rail %q", params.Rail).WithParam("rail")
		}
	}
	for _, p := range slices.Backward(j.psps) {
		if p.Environment != s.providerEnvironment || (rail != "" && p.Rail != rail) || (params.Archived != nil && p.Archived != *params.Archived) {
			continue
		}
		if afterAt != nil && afterID != nil && !(p.CreatedAt.Before(*afterAt) || p.CreatedAt.Equal(*afterAt) && strings.Compare(p.ID.String(), afterID.String()) < 0) {
			continue
		}
		selected = append(selected, p)
	}
	slices.SortFunc(selected, newestFirst)
	if fetch := int(pagination.Fetch(limit)); len(selected) > fetch {
		selected = selected[:fetch]
	}
	return s.pspPage(ctx, id, pagination.Cut(selected, limit, func(p PSPScope) any {
		return pagination.TimeID{At: p.CreatedAt, ID: p.ID}
	}))
}

func newestFirst(a, b PSPScope) int {
	if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
		return c
	}
	return strings.Compare(b.ID.String(), a.ID.String())
}

// pspPage answers a page of PSPs with their open obligations.
func (s *Service) pspPage(ctx context.Context, id billing.MerchantID, page billing.ListPage[PSPScope]) (billing.ListPage[billing.PSP], error) {
	ids := make([]uuid.UUID, 0, len(page.Items))
	for _, p := range page.Items {
		ids = append(ids, p.ID)
	}
	obligations, err := s.pspOpenObligations(ctx, id, ids)
	if err != nil {
		return billing.ListPage[billing.PSP]{}, err
	}
	out := billing.ListPage[billing.PSP]{Items: make([]billing.PSP, 0, len(page.Items)), Next: page.Next}
	for _, p := range page.Items {
		out.Items = append(out.Items, pspView(p, obligations[p.ID]))
	}
	return out, nil
}

// GetPSP reads one PSP.
func (s *Service) GetPSP(ctx context.Context, id billing.MerchantID, pspID billing.PSPID) (billing.PSP, error) {
	if pspID.IsZero() {
		return billing.PSP{}, ErrPSPNotFound
	}
	scope, ok, err := s.PSPScopeByID(ctx, id, pspID.UUID())
	if err != nil {
		return billing.PSP{}, err
	}
	if !ok {
		return billing.PSP{}, ErrPSPNotFound
	}
	return s.pspWithObligations(ctx, id, scope)
}

// CreatePSP arms a new PSP in the merchant's configuration. Its credentials
// are checked with the provider first, and a merchant's first claim of an
// account must prove control of it. A create repeated after it took effect
// returns the PSP it made.
func (s *Service) CreatePSP(ctx context.Context, id billing.MerchantID, req billing.CreatePSPParams) (billing.PSP, error) {
	if !s.config.Writable() {
		return billing.PSP{}, ErrConfigReadOnly
	}
	rail := normalizeProviderSecretType(string(req.Rail))
	if !supportedRail(rail) {
		return billing.PSP{}, apperr.Invalidf("unknown rail %q", req.Rail).WithParam("rail")
	}
	key := strings.ToLower(strings.TrimSpace(req.Key))
	if !merchantdocs.KeyShape.MatchString(key) {
		return billing.PSP{}, apperr.Invalidf("key must be 1-63 lowercase letters, digits, - or _").WithParam("key")
	}
	accountID := strings.TrimSpace(req.AccountID)
	if accountID == "" {
		return billing.PSP{}, apperr.Invalidf("account_id is required").WithParam("account_id")
	}
	if err := config.ValidateRailAccountID(models.Rail(rail), accountID); err != nil {
		return billing.PSP{}, apperr.Invalidf("%v", err).WithParam("account_id")
	}
	if err := validatePSPSettings(rail, req.Settings, req.Credentials); err != nil {
		return billing.PSP{}, err
	}
	credentials, err := s.normalizeCredentials(ctx, rail, req.Credentials)
	if err != nil {
		return billing.PSP{}, err
	}
	if _, supplied := credentials["webhook_signing_secret_previous"]; supplied {
		return billing.PSP{}, apperr.Invalidf("webhook_signing_secret_previous is retained by rotation and cannot be supplied").WithParam("credentials")
	}
	doc := merchantdocs.PSP{Rail: rail, Environment: s.providerEnvironment, AccountID: accountID, Settings: mergeSettings(nil, req.Settings), Secrets: credentials}
	set, err := s.config.Get(ctx, id)
	if err != nil {
		return billing.PSP{}, err
	}
	if existing, ok := set.PSPs[key]; ok {
		if !samePSP(existing.Value, doc) {
			return billing.PSP{}, ErrPSPKeyTaken
		}
		return s.pspByKey(ctx, id, key)
	}
	// An identity this merchant declared under the same key (imported
	// history) is armed by its document; any other owner refuses.
	if owner, found, err := s.ResolvePSPByIdentity(ctx, rail, s.providerEnvironment, accountID); err != nil {
		return billing.PSP{}, err
	} else if found {
		row, ok, err := s.pspRowByID(ctx, id, owner.ID)
		if err != nil {
			return billing.PSP{}, err
		}
		if owner.MerchantID != id || !ok || !strings.EqualFold(row.Key, key) {
			return billing.PSP{}, fmt.Errorf("PSP %s:%s: %w", rail, accountID, ErrPSPExists)
		}
	}
	if err := s.refuseLiveNMIUnderTestMode(ctx, id, doc); err != nil {
		return billing.PSP{}, err
	}
	probed, err := s.probePaymentProviderCredentials(ctx, id, doc)
	if err != nil {
		return billing.PSP{}, err
	}
	if !probed && claimNeedsProof(rail) {
		return billing.PSP{}, ErrPSPClaimUnproven
	}
	set, err = s.config.PutPSP(ctx, id, key, doc, 0)
	if errors.Is(err, merchantdocs.ErrRevisionMismatch) {
		return billing.PSP{}, ErrPSPKeyTaken
	}
	if err != nil {
		return billing.PSP{}, err
	}
	if why, rejected := set.Rejected[merchantdocs.PSPDoc(key)]; rejected {
		return billing.PSP{}, apperr.Invalidf("the PSP was written but is not served: %s", why)
	}
	return s.afterWrite(ctx, id, key, probed, false)
}

// samePSP reports whether a create repeats the document already held.
func samePSP(a, b merchantdocs.PSP) bool {
	return a.Rail == b.Rail && a.Environment == b.Environment && a.AccountID == b.AccountID && !a.Archived &&
		maps.Equal(a.Secrets, b.Secrets) && fmt.Sprint(a.Settings) == fmt.Sprint(b.Settings)
}

// UpdatePSP changes a PSP's settings, rotates its credentials (checked with
// the provider before anything is written) or archives it. With an expected
// revision a PSP changed since is refused; without one the change merges onto
// the latest revision.
func (s *Service) UpdatePSP(ctx context.Context, id billing.MerchantID, pspID billing.PSPID, req billing.UpdatePSPParams) (billing.PSP, error) {
	if !s.config.Writable() {
		return billing.PSP{}, ErrConfigReadOnly
	}
	if req.ExpectedRevision != nil && *req.ExpectedRevision < 0 {
		return billing.PSP{}, apperr.Invalidf("expected_revision must not be negative").WithParam("expected_revision")
	}
	if req.Archived && (len(req.Settings) > 0 || len(req.Credentials) > 0 || req.RetireWebhookOverlap) {
		return billing.PSP{}, apperr.Invalidf("archived goes alone in its request").WithParam("archived")
	}
	for attempt := 0; ; attempt++ {
		j, err := s.load(ctx, id)
		if err != nil {
			return billing.PSP{}, err
		}
		scope, ok := j.byID(pspID.UUID())
		if !ok {
			return billing.PSP{}, ErrPSPNotFound
		}
		held, ok := j.set.PSPs[strings.ToLower(scope.Key)]
		if !ok || scope.Revision == 0 || held.Revision != scope.Revision {
			// No document names this identity: it is superseded or was
			// removed, and drains.
			return billing.PSP{}, ErrPSPNotFound
		}
		if req.ExpectedRevision != nil && *req.ExpectedRevision != held.Revision {
			return billing.PSP{}, RevisionMismatch("the PSP", *req.ExpectedRevision, held.Revision)
		}
		next := clonePSP(held.Value)
		probed, rotated := false, false
		if req.Archived {
			if next.Archived {
				return s.pspWithObligations(ctx, id, scope)
			}
			if len(j.live(scope.Rail, scope.Environment)) <= 1 && !req.AllowLast {
				return billing.PSP{}, &LastActivePSPError{PSP: billing.PSPID(scope.ID)}
			}
			next.Archived = true
		} else {
			if next.Archived {
				return billing.PSP{}, apperr.Invalidf("an archived PSP is never restored")
			}
			if err := validatePSPSettings(scope.Rail, req.Settings, req.Credentials); err != nil {
				return billing.PSP{}, err
			}
			credentials, err := s.normalizeCredentials(ctx, scope.Rail, req.Credentials)
			if err != nil {
				return billing.PSP{}, err
			}
			if rotated, err = s.applyCredentials(&next, credentials, req.RetireWebhookOverlap); err != nil {
				return billing.PSP{}, err
			}
			next.Settings = mergeSettings(next.Settings, req.Settings)
			if len(credentials) > 0 {
				if err := s.refuseLiveNMIUnderTestMode(ctx, id, next); err != nil {
					return billing.PSP{}, err
				}
				if probed, err = s.probePaymentProviderCredentials(ctx, id, next); err != nil {
					return billing.PSP{}, err
				}
			}
		}
		_, err = s.config.PutPSP(ctx, id, scope.Key, next, held.Revision)
		if errors.Is(err, merchantdocs.ErrRevisionMismatch) {
			if req.ExpectedRevision != nil || attempt >= 2 {
				return billing.PSP{}, ErrRevisionMismatch
			}
			continue
		}
		if err != nil {
			return billing.PSP{}, err
		}
		return s.afterWrite(ctx, id, scope.Key, probed, rotated)
	}
}

// applyCredentials writes rotated credentials into p. A changed webhook
// signing secret keeps the outgoing one verifying for a bounded overlap
// (SEC-29) unless retire ends it at once.
func (s *Service) applyCredentials(p *merchantdocs.PSP, credentials map[string]string, retire bool) (bool, error) {
	if _, supplied := credentials["webhook_signing_secret_previous"]; supplied {
		return false, apperr.Invalidf("webhook_signing_secret_previous is retained by rotation and cannot be supplied").WithParam("credentials")
	}
	if p.Secrets == nil {
		p.Secrets = map[string]string{}
	}
	if p.Settings == nil {
		p.Settings = map[string]any{}
	}
	rotated := false
	if next, ok := credentials["webhook_signing_secret"]; ok && hasWebhookOverlap(p.Rail) {
		current := strings.TrimSpace(p.Secrets["webhook_signing_secret"])
		if current != "" && current != next {
			rotated = true
			if !retire {
				p.Secrets["webhook_signing_secret_previous"] = current
				p.Settings[WebhookOverlapExpiresKey] = s.now().Add(s.overlapWindow()).Format(time.RFC3339)
			}
		}
	}
	if retire {
		delete(p.Secrets, "webhook_signing_secret_previous")
		delete(p.Settings, WebhookOverlapExpiresKey)
	}
	maps.Copy(p.Secrets, credentials)
	return rotated, nil
}

// afterWrite records what a write proved and answers the PSP as written.
func (s *Service) afterWrite(ctx context.Context, id billing.MerchantID, key string, probed, rotatedWebhook bool) (billing.PSP, error) {
	scope, err := s.pspScopeByKeyAny(ctx, id, key)
	if err != nil {
		return billing.PSP{}, err
	}
	if probed || rotatedWebhook {
		if err := s.database.RunInMerchantConn(merchant.WithID(ctx, id), func(ctx context.Context) error {
			q := s.database.Gen(ctx)
			if probed {
				now := s.now()
				if err := q.SetPSPCredentialsValidated(ctx, gen.SetPSPCredentialsValidatedParams{MerchantID: id.UUID(), ID: scope.ID, ValidatedAt: &now}); err != nil {
					return err
				}
				scope.ValidatedAt = &now
			}
			if rotatedWebhook {
				// A supplied signing key carries no proof the managed provider
				// endpoint uses it; only a managed rollover binds one.
				scope.WebhookEndpointID = ""
				return q.SetPSPWebhookEndpoint(ctx, gen.SetPSPWebhookEndpointParams{MerchantID: id.UUID(), ID: scope.ID})
			}
			return nil
		}); err != nil {
			return billing.PSP{}, err
		}
	}
	return s.pspWithObligations(ctx, id, scope)
}

// pspRowByID reads one PSP identity row.
func (s *Service) pspRowByID(ctx context.Context, id billing.MerchantID, pspID uuid.UUID) (gen.BillingPsp, bool, error) {
	var row gen.BillingPsp
	err := s.database.RunInMerchantConn(merchant.WithID(ctx, id), func(ctx context.Context) error {
		var err error
		row, err = s.database.Gen(ctx).GetPSP(ctx, gen.GetPSPParams{MerchantID: id.UUID(), ID: pspID})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.BillingPsp{}, false, nil
	}
	return row, err == nil, err
}

func (s *Service) pspByKey(ctx context.Context, id billing.MerchantID, key string) (billing.PSP, error) {
	scope, err := s.pspScopeByKeyAny(ctx, id, key)
	if err != nil {
		return billing.PSP{}, err
	}
	return s.pspWithObligations(ctx, id, scope)
}

// pspScopeByKeyAny is the PSP its document's key currently names, archived or
// not.
func (s *Service) pspScopeByKeyAny(ctx context.Context, id billing.MerchantID, key string) (PSPScope, error) {
	j, err := s.load(ctx, id)
	if err != nil {
		return PSPScope{}, err
	}
	held, ok := j.set.PSPs[key]
	if !ok {
		return PSPScope{}, ErrPSPNotFound
	}
	for _, p := range j.psps {
		if strings.EqualFold(p.Key, key) && p.Revision == held.Revision && p.Revision != 0 {
			return p, nil
		}
	}
	return PSPScope{}, ErrPSPNotFound
}

// RevisionMismatch refuses an edit naming a revision obj moved past;
// metadata.revision is the current one.
func RevisionMismatch(obj string, expected, current int64) error {
	return apperr.New(http.StatusConflict, billing.CodeRevisionMismatch, fmt.Sprintf("%s is at revision %d, not %d: it changed since it was read", obj, current, expected)).
		WithMetadata(map[string]any{"revision": current})
}

// SetPSPAccount points the PSP document under key at another account: the
// Solana signer identity an operator approved. A file is not written; its
// declaration derives the account itself.
func (s *Service) SetPSPAccount(ctx context.Context, id billing.MerchantID, key, account string) error {
	if !s.config.Writable() {
		return nil
	}
	key = strings.ToLower(strings.TrimSpace(key))
	for attempt := 0; ; attempt++ {
		set, err := s.config.Reload(ctx, id)
		if err != nil {
			return err
		}
		doc, ok := set.PSPs[key]
		if !ok {
			return ErrPSPNotFound
		}
		if doc.Value.AccountID == account {
			return nil
		}
		next := clonePSP(doc.Value)
		next.AccountID = account
		_, err = s.config.PutPSP(ctx, id, key, next, doc.Revision)
		if errors.Is(err, merchantdocs.ErrRevisionMismatch) {
			if attempt >= 2 {
				return ErrRevisionMismatch
			}
			continue
		}
		return err
	}
}

func clonePSP(p merchantdocs.PSP) merchantdocs.PSP {
	p.Settings = maps.Clone(p.Settings)
	p.Secrets = maps.Clone(p.Secrets)
	if p.Signer != nil {
		signer := *p.Signer
		p.Signer = &signer
	}
	return p
}

// normalizeCredentials canonicalizes supplied credential keys against the
// rail's registry, refusing operator-only slots and malformed values.
func (s *Service) normalizeCredentials(ctx context.Context, rail string, in map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(in))
	for key, value := range in {
		normalized, err := NormalizePSPSecretKey(rail, key)
		if err != nil {
			return nil, err
		}
		if k, ok := rails.CredentialKeyFor(models.Rail(rail), normalized); !ok || !k.MerchantWritable {
			return nil, apperr.Invalidf("credential %q is set by the operator, not over the API", normalized).WithParam("credentials")
		}
		value = strings.TrimSpace(value)
		if err := validateCredentialValue(rail, normalized, value); err != nil {
			return nil, apperr.Invalidf("credential %q: %v", normalized, err).WithParam("credentials")
		}
		out[normalized] = value
	}
	return out, nil
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
		if !slices.Contains(allowed, key) {
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
	maps.Copy(out, stored)
	for key, value := range write {
		if text, ok := value.(string); value == nil || (ok && strings.TrimSpace(text) == "") {
			delete(out, key)
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Service) pspWithObligations(ctx context.Context, id billing.MerchantID, scope PSPScope) (billing.PSP, error) {
	obligations, err := s.pspOpenObligations(ctx, id, []uuid.UUID{scope.ID})
	if err != nil {
		return billing.PSP{}, err
	}
	return pspView(scope, obligations[scope.ID]), nil
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

// pspView is the API view of a PSP: credential slots by state, never by
// value.
func pspView(scope PSPScope, openObligations int64) billing.PSP {
	configured := map[string]struct{}{}
	for _, key := range pspCredentialKeys(scope.Rail) {
		if scope.HasSecret(key) {
			configured[key] = struct{}{}
		}
	}
	validatedAt := scope.ValidatedAt
	if !pspValidationCredentialsConfigured(scope.Rail, configured) {
		validatedAt = nil
	}
	credentials := make(map[string]billing.PSPCredential)
	for _, key := range pspCredentialKeys(scope.Rail) {
		_, ok := configured[key]
		credential := billing.PSPCredential{Configured: ok}
		if ok {
			credential.ValidatedAt = credentialValidatedAt(scope.Rail, key, validatedAt)
		}
		credentials[key] = credential
	}
	settings := maps.Clone(scope.Settings)
	if settings == nil {
		settings = map[string]any{}
	}
	return billing.PSP{
		ID:              billing.PSPID(scope.ID),
		Key:             scope.Key,
		Rail:            billing.Rail(scope.Rail),
		Environment:     scope.Environment,
		AccountID:       scope.AccountID,
		Archived:        scope.Archived,
		OpenObligations: openObligations,
		Settings:        settings,
		Credentials:     credentials,
		Revision:        scope.Revision,
		CreatedAt:       scope.CreatedAt,
		UpdatedAt:       scope.UpdatedAt,
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

// refuseLiveNMIUnderTestMode requires a fresh simulated result before arming
// sandbox NMI credentials; a live deployment refuses an account left in test
// mode (SEC-33). Live or indeterminate responses refuse the arm.
func (s *Service) refuseLiveNMIUnderTestMode(ctx context.Context, id billing.MerchantID, p merchantdocs.PSP) error {
	if p.Rail != string(models.RailNMI) {
		return nil
	}
	securityKey := strings.TrimSpace(p.Secrets["security_key"])
	if securityKey == "" {
		return nil
	}
	sandbox := s.providerEnvironment == "test"
	deployment, err := config.NMIEndpointDeployment(p.Settings)
	if err != nil {
		return err
	}
	client, err := nmi.NewAccountClient(id.UUID(), PspID(p.Rail, p.Environment, p.AccountID), p.AccountID, &config.NMIProviderSettings{SecurityKey: securityKey, EndpointDeployment: deployment}, sandbox)
	if err != nil {
		return fmt.Errorf("construct NMI posture qualification client: %w", err)
	}
	if s.nmiWire != nil {
		s.nmiWire(client)
	}
	if s.nmiProbeV5BaseURL != "" {
		client.V5BaseURL = s.nmiProbeV5BaseURL
	}
	check := nmi.CheckTestModeArm
	if !sandbox {
		check = nmi.CheckLiveArm
	}
	if err := check(ctx, client); err != nil {
		return providerCredentialError(fmt.Errorf("merchants: rail %q account %q: %w", p.Rail, p.AccountID, err))
	}
	return nil
}

// DeclarePSP records a PSP identity without a document, for imported billing
// facts attributed to it. It never arms the PSP; an account already declared
// keeps its id, key and discovered state. The environment is the
// deployment's.
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
	case !merchantdocs.KeyShape.MatchString(key):
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
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "psps_key_key" {
			return billing.PSP{}, ErrPSPKeyTaken
		}
		return billing.PSP{}, err
	}
	scope, ok, err := s.PSPScopeByID(ctx, id, row.ID)
	if err != nil {
		return billing.PSP{}, err
	}
	if !ok {
		return billing.PSP{}, ErrPSPNotFound
	}
	return s.pspWithObligations(ctx, id, scope)
}
