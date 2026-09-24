package operator

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	authcore "github.com/open-rails/authkit/embedded"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/pkg/merchant"
)

type emailSender struct{ authcore.EmailSender }
type smsSender struct{ authcore.SMSSender }

func TestAttachOptionsPasswordlessPolicy(t *testing.T) {
	var typedNil *emailSender
	for name, tc := range map[string]struct {
		opts    AttachOptions
		wantErr bool
	}{
		"disabled":                        {AttachOptions{}, false},
		"login with email":                {AttachOptions{PasswordlessLogin: true, EmailSender: emailSender{}}, false},
		"login with sms only":             {AttachOptions{PasswordlessLogin: true, EmailSender: typedNil, SMSSender: smsSender{}}, false},
		"hosted auto-registration":        {AttachOptions{HostedPosture: true, PasswordlessLogin: true, PasswordlessAutoRegistration: true, EmailSender: emailSender{}}, false},
		"auto-registration without login": {AttachOptions{HostedPosture: true, PasswordlessAutoRegistration: true, EmailSender: emailSender{}}, true},
		"auto-registration not hosted":    {AttachOptions{PasswordlessLogin: true, PasswordlessAutoRegistration: true, EmailSender: emailSender{}}, true},
		"login without sender":            {AttachOptions{PasswordlessLogin: true}, true},
		"login with typed-nil sender":     {AttachOptions{PasswordlessLogin: true, EmailSender: typedNil}, true},
	} {
		err := validateAttachOptions(tc.opts)
		require.Equal(t, tc.wantErr, err != nil, "%s: %v", name, err)
	}
}

// Attach refuses before building identity resources: nothing is left
// half-attached when the graph cannot accept a control plane.
func TestAttachRefusesBeforeBuildingResources(t *testing.T) {
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}
	require.ErrorContains(t, Attach(context.Background(), nil, cfg, nil, nil), "required")
	require.ErrorContains(t, Attach(context.Background(), &app.App{}, nil, nil, nil), "required")
	require.ErrorContains(t, Attach(context.Background(), &app.App{}, cfg, nil, nil), "runtime is required")
	require.ErrorContains(t, AttachWithOptions(context.Background(), &app.App{Runtime: &app.Runtime{}}, cfg, nil, AttachOptions{PasswordlessLogin: true}), "sender")

	late := &app.App{Runtime: &app.Runtime{RiverClient: &river.Client[pgx.Tx]{}}}
	require.ErrorContains(t, Attach(context.Background(), late, cfg, nil, nil), "attach before River initialization")
	require.Nil(t, late.ControlPlane)
	require.Nil(t, Get(late))
	require.Nil(t, Get(nil))
	require.Nil(t, NameAuthority(late))
}

// Every operator verb is a wiring error without an attached control plane;
// none falls back to an unscoped pool.
func TestOperatorVerbsRequireControlPlane(t *testing.T) {
	ctx, a, id := context.Background(), &app.App{Runtime: &app.Runtime{}}, merchant.ID(uuid.New())
	calls := map[string]func() error{
		"ProvisionMerchant": func() error { _, err := ProvisionMerchant(ctx, a, ProvisionMerchantRequest{Slug: "shop"}); return err },
		"ProvisionMerchantForRestore": func() error {
			_, err := ProvisionMerchantForRestore(ctx, a, ProvisionMerchantForRestoreRequest{MerchantID: id})
			return err
		},
		"RunBootstrap":            func() error { _, err := RunBootstrap(ctx, a, BootstrapOptions{}); return err },
		"ListMerchantsForSubject": func() error { _, err := ListMerchantsForSubject(ctx, a, "user"); return err },
		"ListMerchantRefs":        func() error { _, err := ListMerchantRefs(ctx, a, []string{"shop"}); return err },
		"ListActiveMerchantIDs":   func() error { _, err := ListActiveMerchantIDs(ctx, a, 10, 0); return err },
		"SetMerchantDisplayName":  func() error { return SetMerchantDisplayName(ctx, a, id, "Shop") },
		"SetMerchantAPIHost":      func() error { return SetMerchantAPIHost(ctx, a, id, "api.shop.example") },
		"GetMerchantAPIHost":      func() error { _, err := GetMerchantAPIHost(ctx, a, id); return err },
		"FleetAnalytics":          func() error { _, err := FleetAnalytics(ctx, a, merchant.ID{}, 30); return err },
		"FleetTimeseries":         func() error { _, err := FleetTimeseries(ctx, a, merchant.ID{}, 12); return err },
		"ListPaymentProviderConfigs": func() error {
			_, err := ListPaymentProviderConfigs(ctx, a, id, "", "")
			return err
		},
		"PlanProviderAccountCutover": func() error {
			_, err := PlanProviderAccountCutover(ctx, a, id, ProviderAccountCutoverQuery{})
			return err
		},
		"CompletePendingMerchantRetirements": func() error { _, err := CompletePendingMerchantRetirements(ctx, a, 10); return err },
		"StandaloneRoutes":                   func() error { _, err := StandaloneRoutes(a); return err },
		"SubjectHasVaultedPaymentMethod": func() error {
			_, err := SubjectHasVaultedPaymentMethod(ctx, a, id, "user")
			return err
		},
	}
	for name, call := range calls {
		err := call()
		require.Error(t, err, name)
		require.Regexp(t, "control plane|Attach", err.Error(), name)
	}

	_, err := MerchantCreationAdmission(nil, MerchantCreationPolicy{FreeAllowance: 1})
	require.Error(t, err)
	for _, allowance := range []int{0, -1} {
		_, err = MerchantCreationAdmission(a, MerchantCreationPolicy{FreeAllowance: allowance})
		require.ErrorContains(t, err, "FreeAllowance must be positive")
	}
	admit, err := MerchantCreationAdmission(a, MerchantCreationPolicy{FreeAllowance: 1,
		HasVaultedPaymentMethod: func(context.Context, string) (bool, error) { return true, nil }})
	require.NoError(t, err)
	require.ErrorContains(t, admit(ctx, "shop", "user"), "control plane unavailable", "an unanswerable admission question refuses")
}

// Fleet money travels as exact decimal strings (docs/money-wire.md); counts
// stay numbers; merchant refs carry the stable UUID.
func TestOperatorWireShapes(t *testing.T) {
	week := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	id, err := merchant.ParseID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	require.NoError(t, err)
	roundTrip(t, FleetSnapshot{
		WindowDays: 30,
		Merchants:  FleetMerchantFunnel{Total: 4, Armed: 3, FirstRevenue: 2, ActiveRevenue: 1},
		Revenue:    []FleetCurrencyRevenue{{Currency: "USD", Payments: 41, SettledAmount: 9007199254740993}},
		Rails:      []FleetRailHealth{{Rail: "nmi", Succeeded: 40, Failed: 1}},
		MRR:        []FleetMRR{{Currency: "JPY", Subscriptions: 3, MonthlyAmount: 297000}},
	}, `{"window_days":30,"merchants":{"total":4,"armed":3,"first_revenue":2,"active_revenue":1},
		"revenue":[{"currency":"USD","payments":41,"settled_amount":"9007199254740993"}],
		"rails":[{"rail":"nmi","succeeded":40,"failed":1,"chargebacks":0}],
		"mrr":[{"currency":"JPY","subscriptions":3,"monthly_amount":"297000"}]}`)
	roundTrip(t, FleetSeries{
		Weeks:  12,
		Points: []FleetWeeklyPoint{{WeekStart: week, NewMerchants: 2, ActiveMerchants: 5, CancelledSubscriptions: 1}},
		Volume: []FleetWeeklyVolume{{WeekStart: week, Currency: "USD", Payments: 7, SettledAmount: 495000000}},
	}, `{"weeks":12,"points":[{"week_start":"2026-09-14T00:00:00Z","new_merchants":2,"active_merchants":5,"cancelled_subscriptions":1}],
		"volume":[{"week_start":"2026-09-14T00:00:00Z","currency":"USD","payments":7,"settled_amount":"495000000"}]}`)
	roundTrip(t, MerchantRef{ID: id, Slug: "shop"}, `{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","slug":"shop"}`)
}

func roundTrip[T any](t *testing.T, value T, want string) {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	require.JSONEq(t, want, string(data))
	var back T
	require.NoError(t, json.Unmarshal(data, &back))
	require.Equal(t, value, back)
}
