//go:build integration

package controlplane_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	authcore "github.com/open-rails/authkit/embedded"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/embedded"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

// captureEmailSender implements the authkit/embedded EmailSender alias the way
// an external host would: it records the verification code / reset link
// instead of sending.
type captureEmailSender struct {
	mu         sync.Mutex
	codes      map[string]string // normalized email -> last verification code
	resetLinks map[string]string // normalized email -> last password reset link URL
}

func (s *captureEmailSender) SendVerification(_ context.Context, email, _ string, msg authcore.VerificationMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.codes == nil {
		s.codes = map[string]string{}
	}
	s.codes[email] = msg.Code
	return nil
}
func (s *captureEmailSender) SendPasswordResetLink(_ context.Context, email, _, resetURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resetLinks == nil {
		s.resetLinks = map[string]string{}
	}
	s.resetLinks[email] = resetURL
	return nil
}
func (s *captureEmailSender) SendAccountRegistrationInvite(context.Context, string, string) error {
	return nil
}
func (s *captureEmailSender) SendLoginCode(context.Context, string, string, string) error { return nil }
func (s *captureEmailSender) SendWelcome(context.Context, string, string) error           { return nil }

func (s *captureEmailSender) code(email string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.codes[email]
}

func (s *captureEmailSender) resetLink(email string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resetLinks[email]
}

func hostedTestConfig(dsn, issuer string) *config.Config {
	return &config.Config{
		Env:      "dev",
		TestMode: config.CredentialPostureSandbox,
		// MODE 2 (#723): the hosted-embedder shape — merchants are created over
		// code paths (ProvisionMerchant), not a manifest.
		MerchantSource: config.MerchantSourceAPI,
		SecretBackend:  config.SecretBackendDB,
		DB:             &config.DBConfig{URL: dsn},
		Auth:           &config.AuthConfig{Issuer: issuer},
	}
}

func newHostApp(t *testing.T, cfg *config.Config) *embedded.Embedded {
	t.Helper()
	e, err := embedded.New(context.Background(), embedded.Options{Config: cfg, River: embedded.RiverManagedByOpenRails()})
	require.NoError(t, err, "embedded.New")
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e
}

// mountAuthRoutes serves the attached control plane's AuthKit route surface the
// way an external host mounts it (RouteSpecs are native ServeMux patterns).
func mountAuthRoutes(t *testing.T, e *embedded.Embedded) *httptest.Server {
	t.Helper()
	cp := embcp.Get(e.App())
	require.NotNil(t, cp, "control plane attached")
	mux := http.NewServeMux()
	for _, spec := range cp.RouteSpecs() {
		mux.Handle(spec.Method+" "+spec.Path, spec.Handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
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

// TestHostedPosture_RegisterVerifyProvision drives the full #738 hosted-embedder
// journey through the exported surface only: hosted attach with a host-supplied
// email sender boots, a user registers and email-verifies over the AuthKit HTTP
// surface, and ProvisionMerchant makes that user the owner of a fresh merchant
// (permission group + linked openrails.merchants directory row), idempotently.
func TestHostedPosture_RegisterVerifyProvision(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	cfg := hostedTestConfig(dsn, "https://hosted.openrails.test")
	e := newHostApp(t, cfg)

	// Hosted posture with NO sender must fail loudly at construction (#536 gap:
	// registration verification is Required and nothing could deliver it).
	err := embcp.AttachWithOptions(ctx, e.App(), cfg, nil, embcp.AttachOptions{HostedPosture: true})
	require.Error(t, err, "hosted posture without a sender must refuse to boot")
	require.Contains(t, err.Error(), "no email or SMS sender")
	var typedNilSender *captureEmailSender
	err = embcp.AttachWithOptions(ctx, e.App(), cfg, nil, embcp.AttachOptions{
		HostedPosture: true,
		EmailSender:   typedNilSender,
	})
	require.Error(t, err, "hosted posture with a typed-nil sender must refuse to boot")
	require.Contains(t, err.Error(), "no email or SMS sender")

	// With a host-owned sender, hosted posture boots.
	sender := &captureEmailSender{}
	require.NoError(t, embcp.AttachWithOptions(ctx, e.App(), cfg, nil, embcp.AttachOptions{
		HostedPosture: true,
		EmailSender:   sender,
	}))
	srv := mountAuthRoutes(t, e)

	// Register -> pending registration + verification email to the capture sender.
	sfx := strings.ToLower(uuid.NewString()[:8])
	email := "owner-" + sfx + "@example.test"
	username := "owner" + sfx
	status, body := postJSON(t, srv.URL+"/register",
		`{"identifier":"`+email+`","username":"`+username+`","password":"str0ng-horse-battery!"}`)
	require.Equal(t, http.StatusAccepted, status, "register: %v", body)
	code := sender.code(email)
	require.NotEmpty(t, code, "verification code delivered to host sender")

	// Confirm -> the pending registration becomes a real, verified user.
	status, body = postJSON(t, srv.URL+"/email/verify/confirm",
		`{"email":"`+email+`","code":"`+code+`"}`)
	require.Equal(t, http.StatusOK, status, "verify confirm: %v", body)

	cp := embcp.Get(e.App())
	user, err := cp.Core().GetUserByEmail(ctx, email)
	require.NoError(t, err)

	// Registration is provisioning (openrails-saas #2/#3): the verified user
	// becomes the owner of a freshly provisioned merchant.
	slug := "saas-" + sfx
	res1, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{
		Slug:        slug,
		OwnerUserID: user.ID,
	})
	require.NoError(t, err)
	require.True(t, res1.Created, "first provision creates the merchant")
	require.NotEmpty(t, res1.GroupID)
	require.False(t, res1.MerchantID.IsZero())

	// Directory row exists and records the merchant group's id (#567).
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	var rowID, rowGroupID string
	var rowDisplayName *string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id::text, permission_group_id, display_name FROM openrails.merchants WHERE slug = $1 AND deleted_at IS NULL`,
		slug).Scan(&rowID, &rowGroupID, &rowDisplayName))
	require.Equal(t, res1.MerchantID.String(), rowID)
	require.Equal(t, res1.GroupID, rowGroupID)
	require.Nil(t, rowDisplayName)

	require.NoError(t, embcp.SetMerchantDisplayName(ctx, e.App(), res1.MerchantID, "  SaaS Merchant  "))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT display_name FROM openrails.merchants WHERE id = $1::uuid`, res1.MerchantID.String()).Scan(&rowDisplayName))
	require.NotNil(t, rowDisplayName)
	require.Equal(t, "SaaS Merchant", *rowDisplayName)
	require.NoError(t, embcp.SetMerchantDisplayName(ctx, e.App(), res1.MerchantID, "Repaired Merchant"))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT display_name FROM openrails.merchants WHERE id = $1::uuid`, res1.MerchantID.String()).Scan(&rowDisplayName))
	require.NotNil(t, rowDisplayName)
	require.Equal(t, "Repaired Merchant", *rowDisplayName)
	require.ErrorIs(t,
		embcp.SetMerchantDisplayName(ctx, e.App(), merchant.ID(uuid.New()), "Missing"),
		embcp.ErrMerchantNotFound)

	// The owner holds the merchant owner role (= merchant:*), never platform authority.
	canRead, err := cp.Core().Can(ctx, user.ID, authcore.SubjectKindUser, "merchant", slug, permissions.MerchantSettingsRead)
	require.NoError(t, err)
	require.True(t, canRead, "provisioned owner should hold merchant permissions")

	// Second call is idempotent: same ids, nothing recreated, owner role stable.
	res2, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{
		Slug:        slug,
		OwnerUserID: user.ID,
	})
	require.NoError(t, err)
	require.False(t, res2.Created, "re-run must not recreate the merchant")
	require.Equal(t, res1.MerchantID, res2.MerchantID)
	require.Equal(t, res1.GroupID, res2.GroupID)
	stillOwner, err := cp.Core().Can(ctx, user.ID, authcore.SubjectKindUser, "merchant", slug, permissions.MerchantSettingsRead)
	require.NoError(t, err)
	require.True(t, stillOwner)

	// Provisioning without an attached control plane is a wiring error.
	bare := newHostApp(t, hostedTestConfig(dsn, "https://bare.openrails.test"))
	_, err = embcp.ProvisionMerchant(ctx, bare.App(), embcp.ProvisionMerchantRequest{Slug: "never-" + sfx})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no control plane attached")
}

// TestProvisionMerchant_ConcurrentFirstCreateRace proves the #750 race design
// end-to-end: two goroutines calling ProvisionMerchant for the SAME fresh slug
// at once must both succeed with the identical MerchantID/GroupID, and
// exactly one observes Created=true. ProvisionMerchant's retry-resolve (see
// its doc comment) — not a separate ErrSlugTaken sentinel — converts the race
// loser into the ordinary idempotent path. Repeated across several fresh
// slugs so the goroutine-scheduling race is likely to actually overlap inside
// the DB (CreatePermissionGroup's unique-constraint conflict).
func TestProvisionMerchant_ConcurrentFirstCreateRace(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	cfg := hostedTestConfig(dsn, "https://race.openrails.test")
	e := newHostApp(t, cfg)
	require.NoError(t, embcp.Attach(ctx, e.App(), cfg, nil))

	// Warm up the root group/containment (idempotent, but not the race under
	// test) with a throwaway slug first, so every iteration below races ONLY
	// on the merchant group's own creation — matching how a real deployment's
	// bootstrap already runs ahead of user-triggered provisioning.
	_, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{
		Slug: "race-warmup-" + strings.ToLower(uuid.NewString()[:8]),
	})
	require.NoError(t, err)

	const iterations = 5
	for i := 0; i < iterations; i++ {
		slug := "race-" + strings.ToLower(uuid.NewString()[:8])

		var wg sync.WaitGroup
		results := make([]*embcp.ProvisionMerchantResult, 2)
		errs := make([]error, 2)
		start := make(chan struct{})
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-start
				results[idx], errs[idx] = embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{
					Slug: slug,
				})
			}(g)
		}
		close(start)
		wg.Wait()

		require.NoError(t, errs[0], "iteration %d goroutine 0", i)
		require.NoError(t, errs[1], "iteration %d goroutine 1", i)
		require.Equal(t, results[0].MerchantID, results[1].MerchantID, "iteration %d: same merchant id", i)
		require.Equal(t, results[0].GroupID, results[1].GroupID, "iteration %d: same group id", i)
		require.NotEqual(t, results[0].Created, results[1].Created, "iteration %d: exactly one Created=true", i)
	}
}

// TestSelfHostedPosture_RegistrationStaysClosed guards the default posture: a
// plain Attach (no options, no sender) still boots, and the public registration
// surface is not mounted.
func TestSelfHostedPosture_RegistrationStaysClosed(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	cfg := hostedTestConfig(dsn, "https://selfhosted.openrails.test")
	e := newHostApp(t, cfg)

	require.NoError(t, embcp.Attach(ctx, e.App(), cfg, nil))
	require.True(t, embcp.Get(e.App()).SelfHostedPosture())
	srv := mountAuthRoutes(t, e)

	status, _ := postJSON(t, srv.URL+"/register",
		`{"identifier":"nobody@example.test","username":"nobody","password":"str0ng-horse-battery!"}`)
	require.Equal(t, http.StatusNotFound, status, "self-hosted posture must not mount /register")
}

func (*captureEmailSender) SendContactChanged(context.Context, string, string, authcore.ContactChange) error {
	return nil
}
