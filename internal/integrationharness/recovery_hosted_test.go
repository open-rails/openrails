//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/open-rails/openrails/pkg/embedded"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authhttp"
	authcore "github.com/open-rails/authkit/embedded"
	"github.com/open-rails/authkit/ratelimit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/embed/controlplane"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Hosted is the SaaS-style deployment (Server 3): ONE shared multi-merchant
// engine (embed.New, merchant_source=api) with the OpenRails control plane
// attached in hosted posture and its full HTTP surface — AuthKit under /auth,
// /v1/merchant/*, /v1/me/* — served over httptest. This is the shape
// openrails-saas mounts (internal/engine/engine.go): merchants are provisioned
// for registered, email-verified owners; owners mint API keys through the
// hosted AuthKit routes; the host itself binds in-process Clients per merchant.
// Stop/Start rebuild the whole runtime graph on the same database behind the
// same URL, the way an orchestrator restarts one hosted process.
type Hosted struct {
	Name    string
	BaseURL string
	// Sender captures AuthKit verification mail (codes keyed by email).
	Sender *CaptureSender

	h        *Harness
	server   *httptest.Server
	currency string
	cfg      hostedConfig
	handler  atomic.Pointer[http.Handler]

	mu           sync.Mutex
	runtime      *embed.Runtime
	controlPlane *controlplane.ControlPlane
	keysPath     string
}

// HostedOption customizes the hosted deployment before it boots.
type HostedOption func(*hostedConfig)

type hostedConfig struct {
	workers        bool
	configMutators []func(*config.Config)
}

// HostedWithWorkers runs the River workers inside the hosted runtime.
func HostedWithWorkers() HostedOption { return func(c *hostedConfig) { c.workers = true } }

// HostedWithConfig mutates the hosted engine config before every boot.
func HostedWithConfig(mutate func(*config.Config)) HostedOption {
	return func(c *hostedConfig) {
		if mutate != nil {
			c.configMutators = append(c.configMutators, mutate)
		}
	}
}

// StartHosted boots the hosted deployment over the shared Postgres and Redis.
func (h *Harness) StartHosted(currency string, opts ...HostedOption) *Hosted {
	h.t.Helper()
	s := &Hosted{Name: "saas", h: h, currency: currency, Sender: &CaptureSender{codes: map[string]string{}}, keysPath: h.t.TempDir()}
	for _, opt := range opts {
		if opt != nil {
			opt(&s.cfg)
		}
	}
	s.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handler := s.handler.Load(); handler != nil {
			(*handler).ServeHTTP(w, r)
			return
		}
		http.Error(w, "hosted deployment is stopped", http.StatusServiceUnavailable)
	}))
	s.BaseURL = "http://" + s.server.Listener.Addr().String()
	s.server.Start()
	h.cleanup(s.server.Close)
	s.Start()
	h.cleanup(s.Stop)
	return s
}

// Runtime is the live shared engine (nil while stopped).
func (s *Hosted) Runtime() *embed.Runtime {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runtime
}

// ControlPlane is the attached hosted control plane (nil while stopped).
func (s *Hosted) ControlPlane() *controlplane.ControlPlane {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.controlPlane
}

// AppRuntime is the engine's internal graph, for fixtures only.
func (s *Hosted) AppRuntime() *app.Runtime { return app.HostGraph(s.Runtime()).Runtime }

// Start builds the engine, attaches the hosted control plane and publishes
// the surface. Idempotent while running.
func (s *Hosted) Start() {
	h := s.h
	h.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runtime != nil {
		return
	}
	cfg := &config.Config{
		Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI,
		SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull,
		APIURL: s.BaseURL, CCBillWebhookIPAllowlist: []string{"127.0.0.1/32", "::1/128"},
		DB:   &config.DBConfig{URL: h.DSN},
		Auth: &config.AuthConfig{Issuer: "https://hosted.openrails.test", KeysPath: s.keysPath},
	}
	if h.Redis != nil {
		cfg.Redis = &config.RedisConfig{Addr: h.Redis.Options().Addr}
	}
	for _, mutate := range s.cfg.configMutators {
		mutate(cfg)
	}
	rt, err := embed.New(h.ctx, embed.Options{Options: embedded.Options{Config: cfg, Redis: h.Redis, River: embedded.RiverManagedByOpenRails()}, RunWorkers: s.cfg.workers})
	require.NoError(h.t, err, "hosted embed.New")
	cp, err := controlplane.Attach(h.ctx, rt, controlplane.Options{
		HostedPosture: true, PasswordlessLogin: true, PasswordlessAutoRegistration: true,
		EmailSender: s.Sender, Frontend: authcore.FrontendConfig{BaseURL: s.BaseURL},
		// One loopback peer registers every hosted user in these workflows;
		// AuthKit's per-IP registration cooldown would serialize them a
		// minute apart.
		AuthRateLimitOverrides: map[string]ratelimit.Limit{authhttp.RLAuthRegister: {Limit: 1000, Window: time.Hour}},
	})
	require.NoError(h.t, err, "attach hosted control plane")
	handler, err := cp.Handler()
	require.NoError(h.t, err, "hosted handler")
	s.runtime, s.controlPlane = rt, cp
	s.handler.Store(&handler)
}

// Stop closes the engine (workers, River, pools). Requests answer 503 until
// Start.
func (s *Hosted) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runtime == nil {
		return
	}
	s.handler.Store(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(s.h.t, s.runtime.Close(ctx), "close hosted runtime")
	s.runtime, s.controlPlane = nil, nil
}

// HostedUser is a registered, email-verified hosted account.
type HostedUser struct {
	UserID  string
	Email   string
	Session string
}

// RegisterUser drives the real hosted registration flow: POST /auth/register
// answers 202 pending, the captured verification code confirms, and the
// confirmation issues the session.
func (s *Hosted) RegisterUser(label string) HostedUser {
	h := s.h
	h.t.Helper()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	name := strings.ReplaceAll(strings.ToLower(label), "-", "")
	email := name + "-" + suffix + "@example.test"
	status, body := s.postJSON("/auth/register", "", map[string]string{"identifier": email, "username": name + suffix, "password": "Hosted-proof-2026!"})
	require.Equal(h.t, http.StatusAccepted, status, "register %s: %s", email, body)
	require.Equal(h.t, "verify_email", body["next_action"], "hosted registration is verification-gated")
	code := s.Sender.Code(email)
	require.NotEmpty(h.t, code, "verification code for %s", email)
	status, body = s.postJSON("/auth/verify/confirm", "", map[string]string{"identifier": email, "code": code})
	require.Equal(h.t, http.StatusOK, status, "verify %s: %v", email, body)
	session, _ := body["access_token"].(string)
	require.NotEmpty(h.t, session, "verify confirm issues the session: %v", body)
	user, err := s.ControlPlane().Core().GetUserByEmail(h.ctx, email)
	require.NoError(h.t, err)
	return HostedUser{UserID: user.ID, Email: email, Session: session}
}

// HostedMerchant is one merchant provisioned on the shared engine for its
// owner, integrating through an owner-minted API key.
type HostedMerchant struct {
	ID    merchant.ID
	Slug  string
	Owner HostedUser
	// APIKey is the owner-role key minted over the hosted AuthKit routes.
	APIKey string

	hosted *Hosted
}

// ProvisionMerchant creates the merchant and its AuthKit group for owner
// (what the hosted product's POST /merchants does), then mints the owner's
// integration key through POST /auth/merchant/{slug}/api-keys.
func (s *Hosted) ProvisionMerchant(owner HostedUser, slug string) *HostedMerchant {
	h := s.h
	h.t.Helper()
	slug = merchant.NormalizeSlug(slug)
	created, err := s.ControlPlane().ProvisionMerchant(h.ctx, controlplane.ProvisionMerchantRequest{Slug: slug, OwnerUserID: owner.UserID})
	require.NoError(h.t, err, "provision hosted merchant %s", slug)
	m := &HostedMerchant{ID: created.MerchantID, Slug: slug, Owner: owner, hosted: s}
	m.APIKey = m.MintAPIKey("integration", "owner")
	return m
}

// MintAPIKey mints a key for the merchant through the hosted AuthKit route,
// authenticated by the owner's session.
func (m *HostedMerchant) MintAPIKey(name, role string) string {
	h := m.hosted.h
	h.t.Helper()
	status, body := m.hosted.postJSON("/auth/merchant/"+m.Slug+"/api-keys", m.Owner.Session, map[string]string{"name": name + "-" + uuid.NewString()[:8], "role": role})
	require.Equal(h.t, http.StatusCreated, status, "mint api key for %s: %v", m.Slug, body)
	secret, _ := body["secret"].(string)
	require.NotEmpty(h.t, secret)
	return secret
}

// Client is the shared Client over the hosted HTTP surface, authenticated by
// the owner-minted API key and bound to this merchant.
func (m *HostedMerchant) Client(opts ...openrails.ClientOption) *openrails.Client {
	base := []openrails.ClientOption{
		openrails.WithAPIKey(m.APIKey), openrails.WithMerchantID(m.ID),
		openrails.WithCurrency(m.hosted.currency), openrails.WithTimeout(30 * time.Second),
	}
	client, err := openrails.NewRemote(m.hosted.BaseURL, append(base, opts...)...)
	require.NoError(m.hosted.h.t, err)
	return client
}

// EngineClient is the host's own in-process Client for this merchant — what
// openrails-saas's engine.Client(merchantID) returns.
func (m *HostedMerchant) EngineClient(opts ...openrails.ClientOption) *openrails.Client {
	base := []openrails.ClientOption{openrails.WithMerchantID(m.ID), openrails.WithCurrency(m.hosted.currency)}
	client, err := m.hosted.Runtime().Client(append(base, opts...)...)
	require.NoError(m.hosted.h.t, err)
	return client
}

func (s *Hosted) postJSON(path, bearer string, payload any) (int, map[string]any) {
	h := s.h
	h.t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(h.t, err)
	req, err := http.NewRequestWithContext(h.ctx, http.MethodPost, s.BaseURL+path, bytes.NewReader(raw))
	require.NoError(h.t, err)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(h.t, err)
	decoded := map[string]any{}
	if len(bytes.TrimSpace(body)) > 0 {
		require.NoError(h.t, json.Unmarshal(body, &decoded), "decode %s: %s", path, body)
	}
	return resp.StatusCode, decoded
}

// CaptureSender is the hosted deployment's email sender: it records
// verification codes instead of delivering mail.
type CaptureSender struct {
	mu    sync.Mutex
	codes map[string]string
}

func (s *CaptureSender) Code(email string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.codes[strings.ToLower(email)]
}

func (s *CaptureSender) SendVerification(_ context.Context, email, _ string, msg authcore.VerificationMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[strings.ToLower(email)] = msg.Code
	return nil
}

func (*CaptureSender) SendPasswordResetLink(context.Context, string, string, string) error {
	return nil
}
func (*CaptureSender) SendAccountRegistrationInvite(context.Context, string, string) error {
	return nil
}
func (*CaptureSender) SendLoginCode(context.Context, string, string, string) error { return nil }
func (*CaptureSender) SendWelcome(context.Context, string, string) error           { return nil }
func (*CaptureSender) SendContactChanged(context.Context, string, string, authcore.ContactChange) error {
	return nil
}
func (*CaptureSender) SendDeviceKeyEnrolled(context.Context, string, string, authcore.DeviceKeyNotice) error {
	return nil
}
