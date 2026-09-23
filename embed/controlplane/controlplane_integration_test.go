//go:build integration

package controlplane_test

import (
	"context"
	"encoding/base64"
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
		TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull, MerchantConfigHTTP: true,
		Encryption:    &config.EncryptionConfig{MasterKey: base64.StdEncoding.EncodeToString(make([]byte, 32))},
		SecretBackend: config.SecretBackendDB, DB: &config.DBConfig{URL: dsn},
	}
	rt, err := embed.New(ctx, embed.Options{Config: cfg, River: embed.RiverManagedByOpenRails()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	authConfig := &hostconfig.AuthConfig{AllowMemory: true, AllowEphemeralSigningKey: true, DirectPeerIP: true, Issuer: "https://handle.openrails.test", KeysPath: t.TempDir()}
	_, err = controlplane.Attach(ctx, rt, controlplane.Options{Auth: authConfig, HostedPosture: true})
	require.ErrorContains(t, err, "sender", "hosted posture without a sender refuses to boot")
	_, err = controlplane.Attach(ctx, nil, controlplane.Options{Auth: authConfig})
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
		Auth: authConfig, HostedPosture: true, EmailSender: sender,
		MerchantCreation: &controlplane.MerchantCreationConfig{Admission: admission},
	})
	require.NoError(t, err)
	_, err = controlplane.Attach(ctx, rt, controlplane.Options{Auth: authConfig, HostedPosture: true, EmailSender: sender})
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
	_, err = client.PaymentProviders.Retrieve(ctx, "stripe")
	require.ErrorIs(t, err, openrails.ErrNotFound, "an unconfigured rail is a typed refusal")
	require.ErrorIs(t, cp.SetMerchantDisplayName(ctx, merchant.ID(uuid.New()), "Missing"), controlplane.ErrMerchantNotFound)
	_, err = client.PaymentProviders.Upsert(ctx, "ccbill", &openrails.UpsertPaymentProviderParams{OperationID: uuid.New(), ExpectedRevision: new(int64),
		AccountID: "999983-0000", Credentials: map[string]string{"salt": "handle-fixture"}})
	require.NoError(t, err)
	provider, err := client.PaymentProviders.Retrieve(ctx, "ccbill")
	require.NoError(t, err)
	require.Equal(t, "999983-0000", provider.AccountID)
	encodedProvider, err := json.Marshal(provider)
	require.NoError(t, err)
	require.NotContains(t, string(encodedProvider), "handle-fixture", "credential values never appear in Client readback")
	local, err := rt.Client(openrails.WithMerchantID(created.MerchantID))
	require.NoError(t, err)
	localProvider, err := local.PaymentProviders.Retrieve(ctx, "ccbill")
	require.NoError(t, err)
	require.Equal(t, provider.ID, localProvider.ID)
	require.Equal(t, provider.Revision, localProvider.Revision)

	checkout, err := client.GetCheckoutConfig(ctx)
	require.NoError(t, err)
	require.Len(t, checkout.PSPs, 1)

	// Provider lifecycle through the public Client: list by status, archive by the
	// immutable id with no provider call, last-active refusal, idempotence.
	active, err := client.PaymentProviders.List(ctx, &openrails.PaymentProviderListParams{Provider: "ccbill", Status: "active"})
	require.NoError(t, err)
	require.Len(t, active.Data, 1)
	require.Equal(t, provider.ID, active.Data[0].ID)
	_, err = client.PaymentProviders.Archive(ctx, "ccbill", provider.ID, &openrails.ArchivePaymentProviderAccountParams{})
	var lastActive *openrails.StatusError
	require.ErrorAs(t, err, &lastActive, "the rail's only active account needs the explicit override")
	require.ErrorIs(t, err, openrails.ErrConflict)
	require.Equal(t, "provider_account_last_active", lastActive.Code)
	require.Equal(t, provider.ID.String(), lastActive.Metadata["psp_id"])
	archived, err := client.PaymentProviders.Archive(ctx, "ccbill", provider.ID, &openrails.ArchivePaymentProviderAccountParams{AllowLast: true})
	require.NoError(t, err)
	require.True(t, archived.Archived)
	require.Equal(t, provider.ID, archived.ID)
	repeat, err := client.PaymentProviders.Archive(ctx, "ccbill", provider.ID, &openrails.ArchivePaymentProviderAccountParams{})
	require.NoError(t, err, "archiving an archived account is a no-op")
	require.True(t, repeat.Archived)
	_, err = client.PaymentProviders.Archive(ctx, "ccbill", uuid.New(), &openrails.ArchivePaymentProviderAccountParams{AllowLast: true})
	require.ErrorIs(t, err, openrails.ErrNotFound)
	_, err = client.PaymentProviders.Retrieve(ctx, "ccbill")
	require.ErrorIs(t, err, openrails.ErrNotFound, "no active account remains on the rail")
	drained, err := client.PaymentProviders.List(ctx, &openrails.PaymentProviderListParams{Status: "archived"})
	require.NoError(t, err)
	require.Len(t, drained.Data, 1)
	require.True(t, drained.Data[0].Drained, "archived with no obligations is drained, and still listed")
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
