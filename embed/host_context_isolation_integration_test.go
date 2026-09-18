//go:build integration

package embed_test

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// hostContextObservation is what a host can observe when it calls the Client
// with a context that still carries the identity of the caller IT is serving.
type hostContextObservation struct {
	// Statuses is the HTTP status of each invoice-profile write (0 = success).
	// The admin operation limiter allows ten grant-class writes per human admin
	// per minute; an API-key/host principal is not a human admin.
	Statuses []int
	// AuditUsers are the user ids named by admin rate-limit audit events.
	AuditUsers []string
	// PinMismatchStatus/PinMismatchConflict classify a per-call pin that
	// disagrees with the client's binding.
	PinMismatchStatus   int
	PinMismatchConflict bool
}

// auditHook collects admin audit events emitted through the standard logger.
type auditHook struct {
	mu    sync.Mutex
	users []string
}

func (h *auditHook) Levels() []logrus.Level { return logrus.AllLevels }

func (h *auditHook) Fire(e *logrus.Entry) error {
	if _, ok := e.Data["audit_event"]; !ok {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if user, _ := e.Data["admin_user_id"].(string); user != "" {
		h.users = append(h.users, user)
	}
	return nil
}

func (h *auditHook) drain() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := append([]string(nil), h.users...)
	h.users = nil
	sort.Strings(out)
	return out
}

// TestHostContextNeverReachesTheEngine proves the in-process transport derives
// a fresh engine context: a host request context carrying the session user the
// host is serving (SaaS admin console, embedding site) is metered and audited
// exactly like a standalone API-key request — by the bound merchant, never by
// that user — across the embedded, hosted-HTTP, standalone and multi-merchant
// (SaaS-style) deployments. The bound merchant is still enforced.
func TestHostContextNeverReachesTheEngine(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	standalone := h.StartStandalone("USD")
	host := h.StartEmbeddedHost("USD")
	multi, err := embed.New(ctx, embed.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embed.RiverManagedByOpenRails(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, multi.Close(context.Background())) })
	multiClient, err := multi.Client(openrails.WithMerchantID(dbtest.TestMerchantID))
	require.NoError(t, err)
	local, err := host.Runtime().Client()
	require.NoError(t, err)

	hook := &auditHook{}
	logger := logrus.StandardLogger()
	previous := logger.ReplaceHooks(logrus.LevelHooks{})
	logger.AddHook(hook)
	t.Cleanup(func() { logger.ReplaceHooks(previous) })

	clients := map[string]*openrails.Client{
		"embedded": local, "hosted_http": host.Client(), "standalone": standalone.Client(), "multi_merchant": multiClient,
	}
	observed := map[string]hostContextObservation{}
	for name, client := range clients {
		observed[name] = observeHostContext(t, ctx, h, client, hook)
	}

	want := observed["standalone"]
	require.Equal(t, 11, len(want.Statuses))
	for i, status := range want.Statuses {
		require.Zero(t, status, "standalone write %d must not be limited by a user the engine never authenticated", i+1)
	}
	require.Empty(t, want.AuditUsers, "standalone audit events must not name the host's caller")
	require.Equal(t, 409, want.PinMismatchStatus)
	require.True(t, want.PinMismatchConflict)
	for name, got := range observed {
		require.Equal(t, want, got, "%s attribution diverged from standalone", name)
	}
}

func observeHostContext(t *testing.T, ctx context.Context, h *integrationharness.Harness, client *openrails.Client, hook *auditHook) hostContextObservation {
	t.Helper()
	customer := uuid.New()
	_, err := h.Pool().Exec(ctx, `INSERT INTO openrails.customers(merchant_id,id) VALUES($1,$2)`, dbtest.TestMerchantID.UUID(), customer)
	require.NoError(t, err)

	// What a host handler's context looks like mid-request: the session user
	// the host authenticated, plus the merchant it already resolved.
	sessionUser := uuid.NewString()
	hostCtx := billingauth.SetUserContext(merchant.WithID(ctx, dbtest.TestMerchantID), billingauth.UserContext{
		UserID: sessionUser, Email: "admin@example.com", SessionID: uuid.NewString(),
	})
	hook.drain()

	var out hostContextObservation
	for i := 0; i < 11; i++ {
		err := client.SetCustomerInvoiceProfile(hostCtx, customer.String(), openrails.InvoiceProfileDTO{
			NetTermsDays: i, CollectionMethod: "send_invoice",
		})
		status := 0
		var se *openrails.StatusError
		if errors.As(err, &se) {
			status = se.Status
		} else if err != nil {
			t.Fatalf("write %d: %v", i+1, err)
		}
		out.Statuses = append(out.Statuses, status)
	}
	out.AuditUsers = hook.drain()
	require.NotContains(t, out.AuditUsers, sessionUser, "the host's session user must never be an engine audit actor")

	_, err = client.GetCustomerInvoiceProfile(merchant.WithID(hostCtx, merchant.ID(uuid.New())), customer.String())
	var se *openrails.StatusError
	require.ErrorAs(t, err, &se, "pin mismatch must be a StatusError")
	out.PinMismatchStatus, out.PinMismatchConflict = se.Status, errors.Is(err, openrails.ErrConflict)
	return out
}
