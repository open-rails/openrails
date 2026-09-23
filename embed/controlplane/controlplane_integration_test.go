//go:build integration

package controlplane_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/embed/controlplane"
	hostconfig "github.com/open-rails/openrails/hostauth/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

type captureSender struct{ codes map[string]string }

func (s *captureSender) SendVerification(_ context.Context, email, _ string, msg authcore.VerificationMessage) error {
	if s.codes == nil {
		s.codes = map[string]string{}
	}
	s.codes[strings.ToLower(email)] = msg.Code
	return nil
}
func (*captureSender) SendPasswordResetLink(context.Context, string, string, string) error {
	return nil
}
func (*captureSender) SendAccountRegistrationInvite(context.Context, string, string) error {
	return nil
}
func (*captureSender) SendLoginCode(context.Context, string, string, string) error { return nil }
func (*captureSender) SendWelcome(context.Context, string, string) error           { return nil }
func (*captureSender) SendContactChanged(context.Context, string, string, authcore.ContactChange) error {
	return nil
}
func (*captureSender) SendDeviceKeyEnrolled(context.Context, string, string, authcore.DeviceKeyNotice) error {
	return nil
}

func postJSON(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

// A hosted product attaches the control plane to an embed.Runtime through the
// public handle only: registration and verification over cp.Handler(), the
// owner provisions a merchant, the bootstrap key drives the shared Client over
// the same surface, and an unused merchant retires through the handle.
func TestHostedControlPlaneThroughRuntimeHandle(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	cfg := &config.Config{
		Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantConfigSource: config.MerchantConfigSourceAPI,
		SecretBackend: config.SecretBackendDB, DB: &config.DBConfig{URL: dsn},
	}
	rt, err := embed.New(ctx, embed.Options{Config: cfg, River: embed.RiverManagedByOpenRails()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	_, err = controlplane.Attach(ctx, rt, controlplane.Options{HostedPosture: true})
	require.Error(t, err, "hosted posture without a sender refuses to boot")
	_, err = controlplane.Attach(ctx, nil, controlplane.Options{Auth: &hostconfig.AuthConfig{Issuer: "https://handle.openrails.test", KeysPath: t.TempDir()}})
	require.Error(t, err)

	sender := &captureSender{}
	admission, err := controlplane.MerchantCreationAdmission(rt, controlplane.MerchantCreationPolicy{
		FreeAllowance: 1,
		HasVaultedPaymentMethod: func(ctx context.Context, subject string) (bool, error) {
			return controlplane.SubjectHasVaultedPaymentMethod(ctx, rt, dbtest.TestMerchantID, subject)
		},
	})
	require.NoError(t, err)
	cp, err := controlplane.Attach(ctx, rt, controlplane.Options{
		HostedPosture: true, EmailSender: sender,
		MerchantCreation: &controlplane.MerchantCreationConfig{Admission: admission},
	})
	require.NoError(t, err)
	_, err = controlplane.Attach(ctx, rt, controlplane.Options{HostedPosture: true, EmailSender: sender})
	require.Error(t, err, "a runtime carries one control plane")

	handler, err := cp.Handler()
	require.NoError(t, err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	sfx := strings.ToLower(uuid.NewString()[:8])
	email := "owner-" + sfx + "@example.test"
	status, body := postJSON(t, srv.URL+"/auth/register", `{"identifier":"`+email+`","username":"owner`+sfx+`","password":"str0ng-horse-battery!"}`)
	require.Equal(t, http.StatusAccepted, status, "register: %v", body)
	status, body = postJSON(t, srv.URL+"/auth/verify/confirm", `{"identifier":"`+email+`","code":"`+sender.codes[email]+`"}`)
	require.Equal(t, http.StatusOK, status, "verify: %v", body)
	user, err := cp.Core().GetUserByEmail(ctx, email)
	require.NoError(t, err)

	slug := "handle-" + sfx
	created, err := cp.ProvisionMerchant(ctx, controlplane.ProvisionMerchantRequest{Slug: slug, OwnerUserID: user.ID})
	require.NoError(t, err)
	require.True(t, created.Created)
	again, err := cp.ProvisionMerchant(ctx, controlplane.ProvisionMerchantRequest{Slug: slug, OwnerUserID: user.ID})
	require.NoError(t, err)
	require.False(t, again.Created)
	require.Equal(t, created.MerchantID, again.MerchantID)
	require.ErrorIs(t, admission(ctx, "second-"+sfx, user.ID), controlplane.ErrVaultedPaymentMethodRequired, "past the allowance without a card")
	require.NoError(t, admission(ctx, slug, user.ID), "repairing an owned merchant is not a creation")

	require.NoError(t, cp.SetMerchantDisplayName(ctx, created.MerchantID, "Handle Merchant"))
	refs, err := cp.ListMerchantRefs(ctx, []string{slug})
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.Equal(t, created.MerchantID, refs[0].ID)
	require.Equal(t, "Handle Merchant", refs[0].DisplayName)
	id, canonical, err := cp.ResolveAuthorizedMerchant(ctx, slug, user.ID, permissions.MerchantSettingsRead)
	require.NoError(t, err)
	require.Equal(t, created.MerchantID, id)
	require.Equal(t, slug, canonical)
	_, _, err = cp.ResolveAuthorizedMerchant(ctx, slug, uuid.NewString(), permissions.MerchantSettingsRead)
	require.ErrorIs(t, err, controlplane.ErrPermissionRequired)
	root, err := cp.HasRootPermission(ctx, user.ID, permissions.RootMerchantsRead)
	require.NoError(t, err)
	require.False(t, root, "a merchant owner holds no platform authority")
	require.NoError(t, cp.SetMerchantAPIHost(ctx, created.MerchantID, "handle-"+sfx+".billing.test"))
	host, err := cp.GetMerchantAPIHost(ctx, created.MerchantID)
	require.NoError(t, err)
	require.Equal(t, "handle-"+sfx+".billing.test", host)

	// The bootstrap key drives the shared Client over the same surface.
	boot, err := cp.RunBootstrap(ctx, controlplane.BootstrapOptions{BootstrapMerchantSlug: slug, MintInitialAPIKey: true})
	require.NoError(t, err)
	require.NotEmpty(t, boot.APIKeySecret)
	client, err := openrails.NewRemote(srv.URL, openrails.WithAPIKey(boot.APIKeySecret), openrails.WithMerchantID(created.MerchantID))
	require.NoError(t, err)
	settings, err := client.GetMerchantSettings(ctx)
	require.NoError(t, err)
	require.NotNil(t, settings)
	customer := openrails.CustomerID(uuid.New())
	_, err = client.GrantEntitlement(ctx, (customer).String(), openrails.GrantEntitlementRequest{Entitlement: "premium"})
	require.NoError(t, err)
	members, err := cp.ListMerchantsForSubject(ctx, customer.String())
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, created.MerchantID, members[0].ID)
	_, err = cp.GetPaymentProviderConfig(ctx, created.MerchantID, "stripe", "test")
	require.ErrorIs(t, err, controlplane.ErrPaymentProviderNotFound, "an unconfigured rail is a typed refusal")
	require.ErrorIs(t, cp.SetMerchantDisplayName(ctx, merchant.ID(uuid.New()), "Missing"), controlplane.ErrMerchantNotFound)
	_, err = cp.UpsertPaymentProviderConfig(ctx, created.MerchantID, "ccbill", controlplane.UpsertPaymentProviderConfigRequest{
		AccountID: "999983-0000", Credentials: map[string]string{"salt": "handle-fixture"}})
	require.NoError(t, err)
	provider, err := cp.GetPaymentProviderConfig(ctx, created.MerchantID, "ccbill", "test")
	require.NoError(t, err)
	require.Equal(t, "999983-0000", provider.AccountID)
	checkout, err := client.GetCheckoutConfig(ctx)
	require.NoError(t, err)
	require.Len(t, checkout.PSPs, 1)

	// #655/#656 lifecycle through the handle: list by status, archive by the
	// immutable id with no provider call, last-active refusal, idempotence.
	active, err := cp.ListPaymentProviderConfigs(ctx, created.MerchantID, "ccbill", "active")
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, provider.ID, active[0].ID)
	_, err = cp.ArchivePaymentProviderAccount(ctx, created.MerchantID, "ccbill", provider.ID, controlplane.ArchivePaymentProviderAccountRequest{})
	var lastActive *controlplane.LastActiveProviderAccountError
	require.ErrorAs(t, err, &lastActive, "the rail's only active account needs the explicit override")
	require.Equal(t, provider.ID, lastActive.Account.ID)
	archived, err := cp.ArchivePaymentProviderAccount(ctx, created.MerchantID, "ccbill", provider.ID, controlplane.ArchivePaymentProviderAccountRequest{AllowLast: true})
	require.NoError(t, err)
	require.True(t, archived.Archived)
	require.Equal(t, provider.ID, archived.ID)
	repeat, err := cp.ArchivePaymentProviderAccount(ctx, created.MerchantID, "ccbill", provider.ID, controlplane.ArchivePaymentProviderAccountRequest{})
	require.NoError(t, err, "archiving an archived account is a no-op")
	require.True(t, repeat.Archived)
	_, err = cp.ArchivePaymentProviderAccount(ctx, created.MerchantID, "ccbill", uuid.New(), controlplane.ArchivePaymentProviderAccountRequest{AllowLast: true})
	require.ErrorIs(t, err, controlplane.ErrPaymentProviderAccountNotFound)
	_, err = cp.GetPaymentProviderConfig(ctx, created.MerchantID, "ccbill", "test")
	require.ErrorIs(t, err, controlplane.ErrPaymentProviderNotFound, "no active account remains on the rail")
	drained, err := cp.ListPaymentProviderConfigs(ctx, created.MerchantID, "", "archived")
	require.NoError(t, err)
	require.Len(t, drained, 1)
	require.True(t, drained[0].Drained, "archived with no obligations is drained, and still listed")
	checkout, err = client.GetCheckoutConfig(ctx)
	require.NoError(t, err)
	require.Empty(t, checkout.PSPs, "an archived account is not advertised for new checkout")

	ids, err := cp.ListActiveMerchantIDs(ctx, 1000, 0)
	require.NoError(t, err)
	require.Contains(t, ids, created.MerchantID)
	fleet, err := cp.FleetAnalytics(ctx, merchant.ID{}, 30)
	require.NoError(t, err)
	require.NotNil(t, fleet)

	// Retirement: the merchant with a customer is active and refused; a bare
	// one retires and releases its group.
	var seen bool
	request := controlplane.MerchantRetirementCandidatesRequest{CreatedBefore: time.Now().Add(time.Hour), Limit: 500}
	for !seen {
		page, err := cp.ListMerchantRetirementCandidates(ctx, request)
		require.NoError(t, err)
		for _, candidate := range page.Candidates {
			if candidate.MerchantID == created.MerchantID {
				seen = true
				require.True(t, candidate.Used)
			}
		}
		if page.Next == nil {
			break
		}
		request.After = page.Next
	}
	require.True(t, seen)
	refused, err := cp.RetireUnusedMerchant(ctx, created.MerchantID, created.GroupID)
	require.NoError(t, err)
	require.Equal(t, controlplane.MerchantRetirementRefusedActive, refused.Refusal)
	bare, err := cp.ProvisionMerchant(ctx, controlplane.ProvisionMerchantRequest{Slug: "bare-" + sfx})
	require.NoError(t, err)
	retired, err := cp.RetireUnusedMerchant(ctx, bare.MerchantID, bare.GroupID)
	require.NoError(t, err)
	require.Empty(t, retired.Refusal)
	require.True(t, retired.Retired)
	_, err = cp.Core().GroupInstanceForSlug(ctx, controlplane.MerchantGroup("bare-"+sfx))
	require.ErrorIs(t, err, authkit.ErrGroupNotFound)
	pending, err := cp.CompletePendingMerchantRetirements(ctx, 10)
	require.NoError(t, err)
	require.Zero(t, pending)

	require.NotNil(t, cp.UserAuthenticator())
	jwks := httptest.NewRecorder()
	cp.JWKSHandler().ServeHTTP(jwks, httptest.NewRequest(http.MethodGet, "/jwks", nil))
	require.Equal(t, http.StatusOK, jwks.Code)
}
