//go:build integration

package intents

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

type staticProviderAccountResolver struct {
	identity ProviderAccountIdentity
	ok       bool
}

func (r staticProviderAccountResolver) ResolveProviderAccount(context.Context, string) (ProviderAccountIdentity, bool) {
	return r.identity, r.ok
}

func TestProviderAccountsConstraints(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedPGXPool(t)
	q := gen.New(pool)

	merchantA := uuid.New()
	merchantB := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status)
		 VALUES ($1, $2, 'active'),
		        ($3, $4, 'active')`,
		merchantA, "pa-"+merchantA.String()[:8],
		merchantB, "pa-"+merchantB.String()[:8])
	require.NoError(t, err)

	now := time.Now().UTC()
	first, err := q.UpsertProviderAccount(ctx, gen.UpsertProviderAccountParams{
		MerchantID:     merchantA,
		ProviderType:   "stripe",
		AccountID:      "acct_same",
		LastVerifiedAt: &now,
	})
	require.NoError(t, err)
	require.Equal(t, "primary", first.Role)

	duplicate, err := q.UpsertProviderAccount(ctx, gen.UpsertProviderAccountParams{
		MerchantID:     merchantA,
		ProviderType:   "stripe",
		AccountID:      "acct_same",
		LastVerifiedAt: &now,
	})
	require.NoError(t, err)
	require.Equal(t, first.ID, duplicate.ID, "same merchant/type/account_id upserts the existing row")

	second, err := q.UpsertProviderAccount(ctx, gen.UpsertProviderAccountParams{
		MerchantID:     merchantA,
		ProviderType:   "stripe",
		AccountID:      "acct_second",
		LastVerifiedAt: &now,
	})
	require.NoError(t, err)
	require.Equal(t, "secondary", second.Role, "new accounts do not silently replace the existing primary")

	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.provider_accounts
		   (merchant_id, provider_type, account_id, role, status)
		 VALUES ($1, 'stripe', 'acct_third', 'primary', 'enabled')`,
		merchantA)
	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "uq_provider_accounts_enabled_primary", pgErr.ConstraintName)

	otherMerchant, err := q.UpsertProviderAccount(ctx, gen.UpsertProviderAccountParams{
		MerchantID:     merchantB,
		ProviderType:   "stripe",
		AccountID:      "acct_same",
		LastVerifiedAt: &now,
	})
	require.NoError(t, err, "same provider account id may be stored under a different merchant")
	require.NotEqual(t, first.ID, otherMerchant.ID)
}

func TestVerifyOrBindPrimaryProviderAccountPromotesConfiguredAccountSwap(t *testing.T) {
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))

	merchantID := uuid.New()
	_, err := dbi.Pool().Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status)
		 VALUES ($1, $2, 'active')`,
		merchantID, "pa-swap-"+merchantID.String()[:8])
	require.NoError(t, err)

	store := NewStore(dbi).WithProviderAccounts(staticProviderAccountResolver{
		ok: true,
		identity: ProviderAccountIdentity{
			ProviderKey:  "stripe",
			ProviderType: "stripe",
			AccountID:    "acct_original",
		},
	})
	bound, err := store.VerifyOrBindPrimaryProviderAccount(ctx, merchantID, "stripe")
	require.NoError(t, err)
	require.Equal(t, "acct_original", bound.AccountID)
	require.Equal(t, "primary", bound.Role)

	store.WithProviderAccounts(staticProviderAccountResolver{
		ok: true,
		identity: ProviderAccountIdentity{
			ProviderKey:  "stripe",
			ProviderType: "stripe",
			AccountID:    "acct_other",
		},
	})
	other, err := store.VerifyOrBindPrimaryProviderAccount(ctx, merchantID, "stripe")
	require.NoError(t, err, "a config selector that now points at another account should promote the new provider account")
	require.Equal(t, "acct_other", other.AccountID)
	require.Equal(t, "primary", other.Role)

	rows, err := dbi.Gen(ctx).ListProviderAccountsForMerchant(ctx, gen.ListProviderAccountsForMerchantParams{
		MerchantID:   merchantID,
		ProviderType: strPtr("stripe"),
	})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	requireProviderAccountRole(t, rows, "acct_original", "legacy")
	requireProviderAccountRole(t, rows, "acct_other", "primary")
}

func TestVerifyOrBindPrimaryProviderAccountStripeUsesHTTPAccountIdentity(t *testing.T) {
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	merchantID := seedProviderAccountTestMerchant(t, ctx, dbi.Pool(), "stripe-http")

	var mu sync.Mutex
	accountByKey := map[string]string{
		"sk_test_original": "acct_original",
		"sk_test_rotated":  "acct_original",
		"sk_test_other":    "acct_other",
	}
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		require.Equal(t, "/v1/account", req.URL.Path)
		key := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		calls++
		accountID := accountByKey[key]
		mu.Unlock()
		if accountID == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":%q}`, accountID)
	}))
	defer srv.Close()

	processors := config.ProcessorSet{
		"stripe": {Type: config.ProcessorTypeStripe, SecretKey: "sk_test_original"},
	}
	resolver := NewRuntimeProviderAccounts(&config.Config{}, processors, nil)
	resolver.StripeBaseURL = srv.URL
	store := NewStore(dbi).WithProviderAccounts(resolver)

	bound, err := store.VerifyOrBindPrimaryProviderAccount(ctx, merchantID, "stripe")
	require.NoError(t, err)
	require.Equal(t, "stripe", bound.ProviderType)
	require.Equal(t, "acct_original", bound.AccountID)
	require.Equal(t, "primary", bound.Role)

	processors["stripe"].SecretKey = "sk_test_rotated"
	rotated, err := store.VerifyOrBindPrimaryProviderAccount(ctx, merchantID, "stripe")
	require.NoError(t, err, "key rotation inside the same Stripe account should not create a mismatch")
	require.Equal(t, bound.ID, rotated.ID)

	processors["stripe"].SecretKey = "sk_test_other"
	other, err := store.VerifyOrBindPrimaryProviderAccount(ctx, merchantID, "stripe")
	require.NoError(t, err, "different Stripe account returned by /v1/account should be recorded and promoted")
	require.Equal(t, "acct_other", other.AccountID)
	require.Equal(t, "primary", other.Role)

	rows, err := dbi.Gen(ctx).ListProviderAccountsForMerchant(ctx, gen.ListProviderAccountsForMerchantParams{
		MerchantID:   merchantID,
		ProviderType: strPtr("stripe"),
	})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	requireProviderAccountRole(t, rows, "acct_original", "legacy")
	requireProviderAccountRole(t, rows, "acct_other", "primary")

	mu.Lock()
	require.Equal(t, 3, calls, "each credential key should be resolved through the HTTP account endpoint")
	mu.Unlock()
}

func TestVerifyOrBindPrimaryProviderAccountNMIUsesHTTPProfileIdentity(t *testing.T) {
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	merchantID := seedProviderAccountTestMerchant(t, ctx, dbi.Pool(), "nmi-http")

	var mu sync.Mutex
	profileByKey := map[string]struct {
		company string
		email   string
	}{
		"sec-original": {"Acme Original", "billing@original.test"},
		"sec-rotated":  {"Acme Original", "billing@original.test"},
		"sec-other":    {"Acme Other", "billing@other.test"},
	}
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		require.NoError(t, req.ParseForm())
		require.Equal(t, "profile", req.Form.Get("report_type"))
		key := req.Form.Get("security_key")
		mu.Lock()
		calls++
		profile, ok := profileByKey[key]
		mu.Unlock()
		if !ok {
			_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response><error_response>Specified API key not found</error_response></nm_response>`))
			return
		}
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><nm_response><merchant><company>%s</company><email>%s</email></merchant></nm_response>`, profile.company, profile.email)
	}))
	defer srv.Close()

	client := &nmi.NMIClient{SecurityKey: "sec-original", QueryURL: srv.URL}
	resolver := NewRuntimeProviderAccounts(nil, nil, map[string]*nmi.NMIClient{"mobius": client})
	store := NewStore(dbi).WithProviderAccounts(resolver)

	bound, err := store.VerifyOrBindPrimaryProviderAccount(ctx, merchantID, "mobius")
	require.NoError(t, err)
	require.Equal(t, "nmi", bound.ProviderType)
	require.Equal(t, "Acme Original <billing@original.test>", bound.AccountID)
	require.Equal(t, "primary", bound.Role)

	client.SecurityKey = "sec-rotated"
	rotated, err := store.VerifyOrBindPrimaryProviderAccount(ctx, merchantID, "mobius")
	require.NoError(t, err, "key rotation inside the same NMI account should not create a mismatch")
	require.Equal(t, bound.ID, rotated.ID)

	client.SecurityKey = "sec-other"
	other, err := store.VerifyOrBindPrimaryProviderAccount(ctx, merchantID, "mobius")
	require.NoError(t, err, "different NMI profile returned by query.php should be recorded and promoted")
	require.Equal(t, "Acme Other <billing@other.test>", other.AccountID)
	require.Equal(t, "primary", other.Role)

	rows, err := dbi.Gen(ctx).ListProviderAccountsForMerchant(ctx, gen.ListProviderAccountsForMerchantParams{
		MerchantID:   merchantID,
		ProviderType: strPtr("nmi"),
	})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	requireProviderAccountRole(t, rows, "Acme Original <billing@original.test>", "legacy")
	requireProviderAccountRole(t, rows, "Acme Other <billing@other.test>", "primary")

	mu.Lock()
	require.Equal(t, 3, calls, "each security key should be resolved through the NMI profile HTTP endpoint")
	mu.Unlock()
}

func seedProviderAccountTestMerchant(t *testing.T, ctx context.Context, qx gen.DBTX, prefix string) uuid.UUID {
	t.Helper()
	merchantID := uuid.New()
	_, err := qx.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status)
		 VALUES ($1, $2, 'active')`,
		merchantID, prefix+"-"+merchantID.String()[:8])
	require.NoError(t, err)
	return merchantID
}

func strPtr(s string) *string { return &s }

func requireProviderAccountRole(t *testing.T, rows []gen.OpenrailsProviderAccount, accountID, role string) {
	t.Helper()
	for _, row := range rows {
		if row.AccountID == accountID {
			require.Equal(t, role, row.Role)
			return
		}
	}
	require.Failf(t, "missing provider account", "account_id=%s", accountID)
}
