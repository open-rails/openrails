package merchants

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// CustodianConfigurations projects only the selected merchant's own vaults;
// platform custody has a separate deployment-owned configuration boundary.
func (s *Service) CustodianConfigurations(ctx context.Context, merchant billing.MerchantID) ([]billing.Custodian, error) {
	scopes, err := s.ListCustodians(ctx, merchant)
	if err != nil {
		return nil, err
	}
	out := make([]billing.Custodian, 0, len(scopes))
	for _, scope := range scopes {
		if scope.Kind == models.CustodianPlatform {
			continue
		}
		credentials := map[string]billing.PSPCredential{}
		if descriptor, ok := custodians.Get(scope.Kind); ok {
			for _, slot := range descriptor.Secrets {
				if slot.MerchantWritable {
					credentials[slot.Name] = billing.PSPCredential{Configured: scope.Secret(slot.Name) != ""}
				}
			}
		}
		out = append(out, billing.Custodian{Key: scope.Key, Kind: scope.Kind, Environment: scope.Environment, AccountID: scope.AccountID, Settings: maps.Clone(scope.Settings), Credentials: credentials, Archived: scope.Archived, Revision: scope.Revision})
	}
	slices.SortFunc(out, func(a, b billing.Custodian) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

// WithCustodianCredentialTransport replaces only the read-only vendor account
// verification wire. A supplied wire is refused for live custodian claims.
func (s *Service) WithCustodianCredentialTransport(transport http.RoundTripper) {
	s.custodianCredentialTransport = transport
}

func (s *Service) PutCustodianConfiguration(ctx context.Context, mid billing.MerchantID, key string, request billing.PutCustodianParams) (billing.Custodian, error) {
	var zero billing.Custodian
	if !s.config.Writable() {
		return zero, ErrConfigReadOnly
	}
	if !merchantdocs.KeyShape.MatchString(key) || key == config.PlatformCustodianKey {
		return zero, apperr.Invalidf("a merchant custodian key is required").WithParam("key")
	}
	if request.ExpectedRevision < 0 {
		return zero, apperr.Invalidf("expected_revision must not be negative").WithParam("expected_revision")
	}
	if request.Kind != models.CustodianBasisTheory {
		return zero, apperr.Invalidf("self-service custody requires a supported Basis Theory account").WithParam("kind")
	}
	if request.Environment != config.ClassLive && request.Environment != config.ClassTest {
		return zero, apperr.Invalidf("environment must be live or test").WithParam("environment")
	}
	if request.AccountID == "" || strings.TrimSpace(request.AccountID) != request.AccountID {
		return zero, apperr.Invalidf("account_id is required").WithParam("account_id")
	}
	set, err := s.config.Reload(ctx, mid)
	if err != nil {
		return zero, err
	}
	held := set.Custodians[key]
	if held.Revision != request.ExpectedRevision {
		return zero, RevisionMismatch("custodian", request.ExpectedRevision, held.Revision)
	}
	if scope, found, err := s.CustodianScopeByKey(ctx, mid, key); err != nil {
		return zero, err
	} else if found && (scope.Kind != request.Kind || scope.Environment != request.Environment || scope.AccountID != request.AccountID) {
		return zero, apperr.New(http.StatusConflict, billing.CodeResourceConflict, "custodian identity is immutable; configure another key")
	}
	next := merchantdocs.Custodian{Kind: request.Kind, Environment: request.Environment, AccountID: request.AccountID, Settings: maps.Clone(held.Value.Settings), Secrets: maps.Clone(held.Value.Secrets), Archived: held.Value.Archived}
	if next.Archived {
		return zero, apperr.Invalidf("an archived custodian cannot be changed")
	}
	if request.Settings != nil {
		next.Settings = maps.Clone(request.Settings)
	}
	if next.Secrets == nil {
		next.Secrets = map[string]string{}
	}
	descriptor, _ := custodians.Get(next.Kind)
	for name, value := range request.Credentials {
		slot, ok := descriptor.Secret(name)
		if !ok || !slot.MerchantWritable || strings.TrimSpace(value) == "" {
			return zero, apperr.Invalidf("invalid custodian credential slot").WithParam("credentials")
		}
		next.Secrets[name] = strings.TrimSpace(value)
	}
	if err := config.ValidateCustodianEntry(config.CustodianEntry{Key: key, Kind: next.Kind, AccountID: next.AccountID, Settings: next.Settings, SecretKeys: slices.Collect(maps.Keys(next.Secrets))}); err != nil {
		return zero, apperr.Invalidf("custodian settings and required credentials must be complete").WithParam("settings")
	}
	public, _ := next.Settings[custodians.SettingPublicAPIKey].(string)
	if public == next.Secrets[custodians.SecretAPIKey] || public != "" && public == held.Value.Secrets[custodians.SecretAPIKey] {
		return zero, apperr.Invalidf("the public collection key must differ from the private credential").WithParam("settings.public_api_key")
	}
	if s.custodianCredentialTransport != nil && next.Environment != config.ClassTest {
		return zero, apperr.Invalidf("a live custodian cannot use a test credential wire").WithParam("environment")
	}
	baseURL := basistheory.DefaultBaseURL
	if next.Environment == config.ClassTest {
		baseURL = "https://api.test.basistheory.com"
	}
	client, err := basistheory.New(basistheory.Config{APIKey: next.Secrets[custodians.SecretAPIKey], BaseURL: baseURL, ReadOnly: true, Transport: s.custodianCredentialTransport})
	if err != nil {
		return zero, apperr.Invalidf("custodian credential required").WithParam("credentials")
	}
	tenant, err := client.TenantID(ctx)
	if err != nil {
		var refusal *basistheory.APIError
		if errors.As(err, &refusal) && (refusal.Status == http.StatusUnauthorized || refusal.Status == http.StatusForbidden) {
			return zero, apperr.Invalidf("the custodian credential cannot prove control of its account").WithParam("credentials")
		}
		return zero, apperr.New(http.StatusServiceUnavailable, billing.CodeServiceUnavailable, "custodian account verification unavailable")
	}
	if tenant != next.AccountID {
		return zero, apperr.Invalidf("the custodian credential belongs to another account").WithParam("account_id")
	}
	err = s.database.RunInMerchantConn(merchant.WithID(ctx, mid), func(ctx context.Context) error {
		return AssertCustodianUnowned(ctx, s.database.Gen(ctx), mid.UUID(), next.Kind, next.Environment, next.AccountID)
	})
	if errors.Is(err, ErrCustodianOwnedByAnotherMerchant) {
		return zero, apperr.New(http.StatusConflict, billing.CodeResourceConflict, "custodian account is already owned by another merchant")
	}
	if err != nil {
		return zero, err
	}
	written, err := s.config.PutCustodian(ctx, mid, key, next, request.ExpectedRevision)
	if errors.Is(err, merchantdocs.ErrRevisionMismatch) {
		return zero, ErrRevisionMismatch
	}
	if err != nil {
		return zero, err
	}
	if _, rejected := written.Rejected[merchantdocs.CustodianDoc(key)]; rejected {
		return zero, apperr.New(http.StatusConflict, billing.CodeResourceConflict, "custodian configuration could not be applied")
	}
	items, err := s.CustodianConfigurations(ctx, mid)
	if err != nil {
		return zero, err
	}
	for _, item := range items {
		if item.Key == key {
			return item, nil
		}
	}
	return zero, apperr.New(http.StatusServiceUnavailable, billing.CodeServiceUnavailable, "custodian configuration unavailable")
}
