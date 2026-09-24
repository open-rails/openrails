package payments

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
)

// or#827: the settlement feed publishes on money_movement alone, so a real charge must declare it.
func TestMoneyMovementIsDeclared(t *testing.T) {
	t.Parallel()
	refunded := uuid.New()
	for _, tc := range []struct {
		name    string
		p       models.Payment
		want    models.MoneyMovement
		errText string
	}{
		{"undeclared completed charge", models.Payment{Amount: 7_000_000, Status: "completed"}, "", "must be declared"},
		{"empty status is completed", models.Payment{Amount: 7_000_000}, "", "must be declared"},
		{"declared rail", models.Payment{Amount: 7_000_000, MoneyMovement: models.MoneyMovementRail}, models.MoneyMovementRail, ""},
		{"declared none", models.Payment{Amount: 7_000_000, MoneyMovement: models.MoneyMovementNone}, models.MoneyMovementNone, ""},
		{"invented value", models.Payment{Status: "failed", MoneyMovement: "maybe"}, "", "not a known value"},
		{"declined", models.Payment{Amount: 7_000_000, Status: "failed"}, models.MoneyMovementNone, ""},
		{"pending", models.Payment{Amount: 7_000_000, Status: "pending"}, models.MoneyMovementNone, ""},
		{"reversal", models.Payment{Amount: -7_000_000, RefundedPaymentID: &refunded}, models.MoneyMovementNone, ""},
		{"zero amount", models.Payment{Amount: 0}, models.MoneyMovementNone, ""},
	} {
		got, err := resolveMoneyMovement(&tc.p)
		if tc.errText != "" {
			require.ErrorContains(t, err, tc.errText, tc.name)
			continue
		}
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.want, got, tc.name)
	}
	require.False(t, IsSettlementCandidate(nil))
}

// CUR-6: every minted payment row carries the canonical upper-case currency, or none is minted.
func TestPaymentInsertParamsCanonicalize(t *testing.T) {
	t.Parallel()
	params, err := paymentInsertParams(&models.Payment{ID: uuid.New(), Amount: 1_000_000, Currency: " usd ", MoneyMovement: models.MoneyMovementRail})
	require.NoError(t, err)
	require.Equal(t, "USD", params.Currency)
	require.Equal(t, string(models.MoneyMovementRail), params.MoneyMovement)

	_, err = paymentInsertParams(&models.Payment{Amount: 1_000_000, MoneyMovement: models.MoneyMovementRail})
	require.ErrorContains(t, err, "currency required")
	_, err = paymentInsertParams(&models.Payment{Amount: 1_000_000, Currency: "USD"})
	require.ErrorContains(t, err, "must be declared")
}

// Guards that refuse before the refunded total is ever read.
func TestValidateRefundRefusals(t *testing.T) {
	t.Parallel()
	svc := &PaymentService{}
	refunded := uuid.New()
	mk := func(mut func(*models.Payment)) *models.Payment {
		p := &models.Payment{ID: uuid.New(), Amount: 1000, Status: "completed", MoneyMovement: models.MoneyMovementRail}
		mut(p)
		return p
	}
	for _, tc := range []struct {
		name    string
		p       *models.Payment
		amount  int64
		errText string
	}{
		{"nil", nil, 500, "required"},
		{"zero amount", mk(func(*models.Payment) {}), 0, "> 0"},
		{"failed", mk(func(p *models.Payment) { p.Status = "failed" }), 500, "completed"},
		{"pending", mk(func(p *models.Payment) { p.Status = "pending" }), 500, "completed"},
		{"refunded", mk(func(p *models.Payment) { p.Status = "refunded" }), 500, "completed"},
		{"reversal row", mk(func(p *models.Payment) { p.Amount, p.RefundedPaymentID = -1000, &refunded }), 500, "successful charge"},
		{"bookkeeping row", mk(func(p *models.Payment) { p.MoneyMovement = models.MoneyMovementNone }), 500, "no money movement"},
	} {
		require.ErrorContains(t, svc.ValidateRefund(context.Background(), tc.p, tc.amount), tc.errText, tc.name)
	}

	for status, want := range map[string]bool{"": true, "completed": true, " Completed ": true, "pending": false, "failed": false} {
		require.Equal(t, want, PaymentStatusCompleted(status), status)
	}
}

// Pending reservations hold refund capacity; failed ones release it; recoveries net out, never below zero.
func TestEffectiveRefundTotal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		rows []refundTotalRow
		want int64
	}{
		{nil, 0},
		{[]refundTotalRow{{Amount: -500, Status: "completed"}}, 500},
		{[]refundTotalRow{{Amount: -500, Status: "pending"}}, 500},
		{[]refundTotalRow{{Amount: -500, Status: " FAILED "}}, 0},
		{[]refundTotalRow{{Amount: -500}, {Amount: -200}}, 700},
		{[]refundTotalRow{{Amount: -500}, {Amount: 300}}, 200},
		{[]refundTotalRow{{Amount: -500}, {Amount: 700}}, 0},
	} {
		require.Equal(t, tc.want, effectiveRefundTotalFromLinkedRows(tc.rows), "%+v", tc.rows)
	}
}

func TestRailHasRemoteCustomer(t *testing.T) {
	t.Parallel()
	for rail, want := range map[string]bool{"stripe": true, " Stripe ": true, "nmi": false, "mobius": false, "ccbill": false, "solana": false, "": false} {
		require.Equal(t, want, railHasRemoteCustomer(rail), rail)
	}
}

func TestStripeCardNormalization(t *testing.T) {
	t.Parallel()
	require.Equal(t, &StripeCard{Brand: "Visa", Last4: "4242", Expiry: "03/30"}, NormalizeStripeCard(StripeCardDetails{Brand: " visa ", Last4: " 4242 ", ExpMonth: 3, ExpYear: 2030}))
	require.Equal(t, &StripeCard{Brand: "Amex", Last4: "0005"}, NormalizeStripeCard(StripeCardDetails{Brand: "amex", Last4: "0005", ExpMonth: 3}))
	require.Nil(t, NormalizeStripeCard(StripeCardDetails{Brand: "visa", ExpMonth: 1, ExpYear: 2030}))
	require.Equal(t, "", TitleCaseBrand("  "))
	require.Equal(t, []string{"ch_1", "pi_1"}, compactStrings(" ch_1 ", "", "pi_1", "ch_1"))
}

// A sale operation is recovered only from its own accepted terms; any contradiction refuses it.
func TestDecodeNMISalePayload(t *testing.T) {
	t.Parallel()
	psp, price := uuid.New(), uuid.New()
	accepted := time.Date(2026, 5, 1, 12, 0, 0, 123456000, time.UTC)
	hours := 720
	end := accepted.Add(720 * time.Hour)
	valid := func() NMISalePayload {
		return NMISalePayload{
			RequestFingerprint: strings.Repeat("ab", 32), Provider: "nmi", PSP: "nmi", Amount: 9_990_000, Currency: "USD",
			UserID: uuid.NewString(), PriceID: price, PaymentMethodID: uuid.New(), PaymentID: uuid.New(), ProductID: uuid.New(),
			Instrument: charge.FrozenInstrument{PSPID: psp, Custodian: models.CustodianPSP, RailCustomerRef: "vault"},
			AcceptedAt: accepted, EntitlementStart: accepted, OwnershipStart: accepted, Entitlements: map[string]*int{},
			AccessDurationHours: &hours, OwnershipEnd: &end, Eligibility: "allowed",
		}
	}
	intent := func(p NMISalePayload, mut func(*gen.OpenrailsRailIntent)) gen.OpenrailsRailIntent {
		raw, err := json.Marshal(p)
		require.NoError(t, err)
		in := gen.OpenrailsRailIntent{ID: uuid.New(), MerchantID: uuid.New(), Rail: "nmi", IntentType: TypeNMISale, PspID: &psp, PriceID: &price, Payload: raw}
		if mut != nil {
			mut(&in)
		}
		return in
	}

	got, err := DecodeNMISalePayload(intent(valid(), nil))
	require.NoError(t, err)
	require.Equal(t, int64(9_990_000), got.Amount)
	indefinite := valid()
	indefinite.AccessDurationHours, indefinite.OwnershipEnd = nil, nil
	_, err = DecodeNMISalePayload(intent(indefinite, nil))
	require.NoError(t, err)

	otherID := uuid.New()
	for name, mut := range map[string]func(*gen.OpenrailsRailIntent){
		"wrong type":       func(in *gen.OpenrailsRailIntent) { in.IntentType = "refund" },
		"unsupported rail": func(in *gen.OpenrailsRailIntent) { in.Rail = "ccbill" },
		"other account":    func(in *gen.OpenrailsRailIntent) { in.PspID = &otherID },
		"other price":      func(in *gen.OpenrailsRailIntent) { in.PriceID = &otherID },
		"custodian set":    func(in *gen.OpenrailsRailIntent) { in.CustodianID = &otherID },
		"garbage payload":  func(in *gen.OpenrailsRailIntent) { in.Payload = []byte(`{`) },
	} {
		_, err := DecodeNMISalePayload(intent(valid(), mut))
		require.Error(t, err, name)
	}
	for name, mut := range map[string]func(*NMISalePayload){
		"lowercase currency":    func(p *NMISalePayload) { p.Currency = "usd" },
		"sub-cent amount":       func(p *NMISalePayload) { p.Amount = 9_995_000 },
		"zero amount":           func(p *NMISalePayload) { p.Amount = 0 },
		"bad user":              func(p *NMISalePayload) { p.UserID = "user" },
		"non-db precision":      func(p *NMISalePayload) { p.AcceptedAt = p.AcceptedAt.Add(1) },
		"uppercase fingerprint": func(p *NMISalePayload) { p.RequestFingerprint = strings.ToUpper(p.RequestFingerprint) },
		"short fingerprint":     func(p *NMISalePayload) { p.RequestFingerprint = "abcd" },
		"not eligible":          func(p *NMISalePayload) { p.Eligibility = "denied" },
		"ownership mismatch":    func(p *NMISalePayload) { e := p.OwnershipEnd.Add(time.Hour); p.OwnershipEnd = &e },
		"finite end, no hours":  func(p *NMISalePayload) { p.AccessDurationHours = nil },
		"entitlement too early": func(p *NMISalePayload) { p.EntitlementStart = p.AcceptedAt.Add(-time.Hour) },
		"custodian card": func(p *NMISalePayload) {
			id := uuid.New()
			p.Instrument.Custodian, p.Instrument.CustodianID = models.CustodianBasisTheory, &id
		},
		"provider mismatch": func(p *NMISalePayload) { p.Provider = "stripe" },
		"no customer ref":   func(p *NMISalePayload) { p.Instrument.RailCustomerRef = "" },
		"nil entitlements":  func(p *NMISalePayload) { p.Entitlements = nil },
	} {
		p := valid()
		mut(&p)
		_, err := DecodeNMISalePayload(intent(p, nil))
		require.Error(t, err, name)
	}

	stripe := valid()
	stripe.Provider = "stripe"
	_, err = DecodeNMISalePayload(intent(stripe, func(in *gen.OpenrailsRailIntent) { in.Rail = "stripe" }))
	require.Error(t, err, "a Stripe sale needs the exact pm_")
	stripe.Instrument.RailMethodRef = "pm_1"
	_, err = DecodeNMISalePayload(intent(stripe, func(in *gen.OpenrailsRailIntent) { in.Rail = "stripe" }))
	require.NoError(t, err)

	id := uuid.New()
	require.Equal(t, id.String(), NMISaleOrderReference(id, " "))
	ref := NMISaleOrderReference(id, "run-1")
	require.True(t, strings.HasPrefix(ref, id.String()+"_e2e_"))
	require.Len(t, ref, len(id.String())+len("_e2e_")+8)
	require.NotEqual(t, ref, NMISaleOrderReference(id, "run-2"))
}
