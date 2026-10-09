package operator

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/controlplane"
)

func TestOptionsPasswordlessPolicy(t *testing.T) {
	outbox := new(authtest.Outbox)
	emailSender, smsSender := outbox.Email(), outbox.SMS()
	for name, tc := range map[string]struct {
		opts    Options
		wantErr bool
	}{
		"disabled":                        {Options{}, false},
		"login with email":                {Options{LocalSignIn: true, PasswordlessLogin: true, EmailSender: emailSender}, false},
		"login with sms only":             {Options{LocalSignIn: true, PasswordlessLogin: true, SMSSender: smsSender}, false},
		"open auto-registration":          {Options{LocalSignIn: true, Registration: iam.RegistrationModeOpen, PasswordlessLogin: true, PasswordlessAutoRegistration: true, EmailSender: emailSender}, false},
		"auto-registration without login": {Options{LocalSignIn: true, Registration: iam.RegistrationModeOpen, PasswordlessAutoRegistration: true, EmailSender: emailSender}, true},
		"auto-registration closed":        {Options{LocalSignIn: true, PasswordlessLogin: true, PasswordlessAutoRegistration: true, EmailSender: emailSender}, true},
		"auto-registration invite-only":   {Options{LocalSignIn: true, Registration: iam.RegistrationModeInviteOnly, PasswordlessLogin: true, PasswordlessAutoRegistration: true, EmailSender: emailSender}, true},
		"login without sender":            {Options{LocalSignIn: true, PasswordlessLogin: true}, true},
		"open without sender":             {Options{LocalSignIn: true, Registration: iam.RegistrationModeOpen}, true},
		"invite-only with sender":         {Options{LocalSignIn: true, Registration: iam.RegistrationModeInviteOnly, SMSSender: smsSender}, false},
		"closed without sender":           {Options{LocalSignIn: true, Registration: iam.RegistrationModeClosed}, false},
		"unknown mode":                    {Options{LocalSignIn: true, Registration: "sometimes"}, true},
		"open without local sign-in":      {Options{Registration: iam.RegistrationModeOpen, EmailSender: emailSender}, true},
		"passwordless without local":      {Options{PasswordlessLogin: true, EmailSender: emailSender}, true},
	} {
		_, err := ControlPlaneOptions(tc.opts)
		require.Equal(t, tc.wantErr, err != nil, "%s: %v", name, err)
	}
}

// An unanswerable admission question refuses.
func TestMerchantCreationAdmissionRefusesWithoutAnswers(t *testing.T) {
	_, err := MerchantCreationAdmission(nil, MerchantCreationPolicy{FreeAllowance: 1})
	require.Error(t, err)
	none := func() *controlplane.ControlPlane { return nil }
	for _, allowance := range []int{0, -1} {
		_, err = MerchantCreationAdmission(none, MerchantCreationPolicy{FreeAllowance: allowance})
		require.ErrorContains(t, err, "FreeAllowance must be positive")
	}
	admit, err := MerchantCreationAdmission(none, MerchantCreationPolicy{FreeAllowance: 1,
		HasVaultedPaymentMethod: func(context.Context, string) (bool, error) { return true, nil }})
	require.NoError(t, err)
	require.ErrorContains(t, admit(context.Background(), "shop", "user"), "control plane unavailable")
}

// Fleet money travels as exact decimal strings (docs/money-wire.md); counts
// stay numbers; merchant refs carry the stable UUID.
func TestOperatorWireShapes(t *testing.T) {
	week := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	id, err := billing.ParseMerchantID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	require.NoError(t, err)
	roundTrip(t, billing.FleetSnapshot{
		WindowDays: 30,
		Merchants:  billing.FleetMerchantFunnel{Total: 4, Armed: 3, FirstRevenue: 2, ActiveRevenue: 1},
		Revenue:    []billing.FleetCurrencyRevenue{{Currency: "USD", Payments: 41, SettledAmount: 9007199254740993}},
		Rails:      []billing.FleetRailHealth{{Rail: "nmi", Succeeded: 40, Failed: 1}},
		MRR:        []billing.FleetMRR{{Currency: "JPY", Subscriptions: 3, MonthlyAmount: 297000}},
	}, `{"window_days":30,"merchants":{"total":4,"armed":3,"first_revenue":2,"active_revenue":1},
		"revenue":[{"currency":"USD","payments":41,"settled_amount":"9007199254740993"}],
		"rails":[{"rail":"nmi","succeeded":40,"failed":1,"chargebacks":0}],
		"mrr":[{"currency":"JPY","subscriptions":3,"monthly_amount":"297000"}]}`)
	roundTrip(t, billing.FleetSeries{
		Weeks:  12,
		Points: []billing.FleetWeeklyPoint{{WeekStart: week, NewMerchants: 2, ActiveMerchants: 5, CanceledSubscriptions: 1}},
		Volume: []billing.FleetWeeklyVolume{{WeekStart: week, Currency: "USD", Payments: 7, SettledAmount: 495000000}},
	}, `{"weeks":12,"points":[{"week_start":"2026-09-14T00:00:00Z","new_merchants":2,"active_merchants":5,"canceled_subscriptions":1}],
		"volume":[{"week_start":"2026-09-14T00:00:00Z","currency":"USD","payments":7,"settled_amount":"495000000"}]}`)
	roundTrip(t, billing.MerchantRef{ID: id, Slug: "shop"}, `{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","slug":"shop","display_name":""}`)
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
