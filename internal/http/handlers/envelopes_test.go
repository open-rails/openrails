package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingimport"
	"github.com/open-rails/openrails/internal/catalogpolicy"
	"github.com/open-rails/openrails/internal/db"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	billingservice "github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

func newTestRequest(method, target string, body io.Reader, rt *app.Runtime) (*httprequest.Request, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	return httprequest.NewHTTP(rec, httptest.NewRequest(method, target, body), rt), rec
}

type envelope struct {
	Status int
	api.ErrorDetails
	Raw string
}

func (e envelope) param() string {
	if e.Param == nil {
		return ""
	}
	return *e.Param
}

func render(t *testing.T, write func(*httprequest.Request)) envelope {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Request-ID", "req-envelope")
	write(httprequest.NewHTTP(rec, req, &app.Runtime{}))
	var body api.ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return envelope{Status: rec.Code, ErrorDetails: body.Error, Raw: rec.Body.String()}
}

// #983: status, code and param come from the typed refusal; rewording the
// human message never moves them, and untyped errors are 500s that never leak.
func TestRefusalClassificationIgnoresHumanMessage(t *testing.T) {
	writers := map[string]func(*httprequest.Request, error){
		"catalog":  writeCatalogError,
		"reprice":  writeRepriceError,
		"psp":      writePSPError,
		"metering": writeMeteringError,
		"refusal":  func(r *httprequest.Request, err error) { writeRefusal(r, err, "request failed") },
		"checkout": func(r *httprequest.Request, err error) {
			writeCheckoutAttemptError(r, err, checkoutAttemptErrorContext{})
		},
		"tier": writeChangeTierError,
	}
	type want struct {
		status      int
		code, param string
	}
	cases := []struct {
		writer string
		err    error
		want   want
	}{
		{"catalog", apperr.Invalidf("price key needs disambiguation").WithParam("key"), want{400, api.CodeInvalidParam, "key"}},
		{"catalog", billingservice.ErrProductNotFound, want{404, "product_not_found", ""}},
		{"catalog", billingservice.ErrPriceNotFound, want{404, "price_not_found", ""}},
		{"catalog", billingservice.ErrPriceKeyNotFound, want{404, "price_key_not_found", ""}},
		{"catalog", billingservice.ErrCatalogConflict, want{409, api.CodeResourceConflict, ""}},
		{"catalog", billingservice.ErrProductTierGroupInUse, want{409, "product_tier_group_in_use", ""}},
		{"catalog", fmt.Errorf("%w: price declares a trial", billingservice.ErrTrialUnsupportedOnRail), want{400, "trial_unsupported_on_rail", ""}},
		{"catalog", billingservice.ErrCurrencyUnsupported.WithParam("currency"), want{400, "currency_unsupported", "currency"}},
		{"refusal", subscriptions.ErrSubscriptionNotFound, want{404, "subscription_not_found", ""}},
		{"refusal", subscriptions.ErrSubscriptionNotActive, want{409, "subscription_not_active", ""}},
		{"refusal", fmt.Errorf("%w: %s", subscriptions.ErrCancelUnsupportedOnRail, "carrier_pigeon"), want{400, "cancel_unsupported_on_rail", ""}},
		{"refusal", subscriptions.ErrCustomerActionRequired, want{403, "customer_action_required", ""}},
		{"refusal", fmt.Errorf("%w: row 7", billingimport.ErrInvalidPSPReference), want{400, "invalid_psp_reference", ""}},
		{"refusal", fmt.Errorf("%w: declared field refused", billingimport.ErrInvalidDeclaredInput), want{400, api.CodeInvalidParam, ""}},
		{"refusal", apperr.Conflictf("payment method belongs to another customer"), want{409, api.CodeResourceConflict, ""}},
		{"reprice", subscriptions.ErrSubscriptionNotFound, want{404, "subscription_not_found", ""}},
		{"reprice", subscriptions.ErrRepriceTargetPriceNotFound, want{404, "reprice_target_price_not_found", ""}},
		{"reprice", fmt.Errorf("%w: price key %q", subscriptions.ErrRepricePriceKeyNotFound, "k"), want{404, "reprice_price_key_not_found", ""}},
		{"reprice", subscriptions.ErrRepriceNotFound, want{404, "reprice_not_found", ""}},
		{"reprice", &subscriptions.RepriceConstraintError{Sentinel: subscriptions.ErrRepriceCrossProduct}, want{422, "reprice_cross_product", ""}},
		{"reprice", &subscriptions.RepriceConstraintError{Sentinel: subscriptions.ErrRepriceNoticeWindowViolation}, want{422, "reprice_notice_window_violation", ""}},
		{"reprice", &subscriptions.RepriceConstraintError{Sentinel: subscriptions.ErrRepriceAlreadyScheduled}, want{409, "reprice_already_scheduled", ""}},
		{"reprice", subscriptions.ErrRepriceNotScheduled, want{409, "reprice_not_scheduled", ""}},
		{"psp", apperr.Invalidf("unknown rail %q", "abacus").WithParam("rail"), want{400, api.CodeInvalidParam, "rail"}},
		{"psp", fmt.Errorf("%w: stripe key rejected (401)", merchants.ErrPSPCredentialsRejected), want{400, "psp_credentials_rejected", ""}},
		{"psp", merchants.ErrPSPNotFound, want{404, "psp_not_found", ""}},
		{"psp", merchants.ErrPSPKeyTaken, want{409, "psp_key_taken", ""}},
		{"psp", &merchants.LastActivePSPError{}, want{409, "psp_last_active", ""}},
		{"metering", billingservice.ErrUsageMeterNotFound, want{404, "usage_meter_not_found", ""}},
		{"metering", billingservice.ErrDefaultRateCardNotFound, want{404, "default_rate_card_not_found", ""}},
		{"metering", billingservice.ErrRateCardProductNotFound, want{404, "rate_card_product_not_found", ""}},
		{"metering", billingservice.ErrAllowanceMeterNotFound, want{404, "allowance_meter_not_found", ""}},
		{"metering", billingservice.ErrUsageRateCardInvalid, want{400, "usage_rate_card_invalid", ""}},
		{"metering", errors.Join(errors.New("context"), billingservice.ErrMeterInUse), want{409, "meter_in_use", ""}},
		{"metering", billingservice.ErrMeterRateCardConflict, want{409, "meter_rate_card_conflict", ""}},
		{"metering", billingservice.ErrAllowanceSourceInvalid, want{409, "allowance_source_invalid", ""}},
		{"metering", billingservice.ErrAllowanceSourceInUse, want{409, "allowance_source_in_use", ""}},
		{"metering", billingservice.ErrDefaultRateCardRequired, want{409, "default_rate_card_required", ""}},
		{"metering", billingservice.ErrRateCardHasOverrides, want{409, "rate_card_has_overrides", ""}},
		{"metering", billingservice.ErrRateCardCurrencyMismatch, want{409, "rate_card_currency_mismatch", ""}},
		{"metering", catalogpolicy.ErrUpdatesDisabled, want{403, "catalog_updates_disabled", ""}},
		{"catalog", catalogpolicy.ErrUpdatesDisabled, want{403, "catalog_updates_disabled", ""}},
	}
	reworded := func(err error) error {
		var refusal *apperr.Error
		if !errors.As(err, &refusal) {
			return err
		}
		return fmt.Errorf("outer context changed: %w", &apperr.Error{Status: refusal.Status, Code: refusal.Code, Param: refusal.Param, Message: "REWORDED " + refusal.Message})
	}
	for _, tc := range cases {
		t.Run(tc.writer+"/"+tc.want.code, func(t *testing.T) {
			write := writers[tc.writer]
			for _, err := range []error{tc.err, reworded(tc.err)} {
				got := render(t, func(r *httprequest.Request) { write(r, err) })
				require.Equal(t, tc.want, want{got.Status, got.Code, got.param()}, got.Raw)
			}
		})
	}
	for name, write := range writers {
		for _, err := range []error{
			errors.New(`ERROR: duplicate key value violates unique constraint "x" (SQLSTATE 23505)`),
			errors.New("product not found"),
			errors.New("subscription is not active"),
			errors.New(`money: unknown currency "XXX"`),
		} {
			got := render(t, func(r *httprequest.Request) { write(r, err) })
			require.Equal(t, want{500, api.CodeInternalError, ""}, want{got.Status, got.Code, got.param()}, "%s: %v", name, err)
			require.NotContains(t, got.Raw, err.Error(), "%s leaked internal text", name)
		}
	}
}

func TestRefusalKeepsServerDiagnosticsOutOfResponse(t *testing.T) {
	logger := logrus.StandardLogger()
	hooks := logger.ReplaceHooks(make(logrus.LevelHooks))
	t.Cleanup(func() { logger.ReplaceHooks(hooks) })
	hook := logtest.NewGlobal()
	cause := fmt.Errorf("db: acquire pgx merchant connection: %w: %v", db.ErrPoolExhausted, context.DeadlineExceeded)
	got := render(t, func(r *httprequest.Request) { writeRefusal(r, cause, "customer read failed") })
	assert.Equal(t, http.StatusServiceUnavailable, got.Status)
	assert.Equal(t, db.ErrPoolExhausted.Code, got.Code)
	assert.Equal(t, db.ErrPoolExhausted.Message, got.Message)
	assert.Equal(t, "req-envelope", got.RequestID)
	entries := hook.AllEntries()
	require.Len(t, entries, 1)
	assert.Equal(t, logrus.ErrorLevel, entries[0].Level)
	assert.Equal(t, cause, entries[0].Data[logrus.ErrorKey])
	assert.Equal(t, "req-envelope", entries[0].Data["request_id"])

	validation := fmt.Errorf("%w: row 7", billingimport.ErrInvalidPSPReference)
	got = render(t, func(r *httprequest.Request) { writeRefusal(r, validation, "billing import failed") })
	assert.Equal(t, validation.Error(), got.Message)
}

// Decoder failures are coded, name the field and never carry Go decoder text.
func TestDecodeErrorIsCodedAndNamesTheField(t *testing.T) {
	decode := func(body string) error {
		var out struct {
			Key   string `json:"key"`
			Price int64  `json:"price"`
		}
		d := json.NewDecoder(strings.NewReader(body))
		d.DisallowUnknownFields()
		return d.Decode(&out)
	}
	for _, tc := range []struct {
		err                  error
		status               int
		code, message, param string
	}{
		{decode(`{"credits_spec":{}}`), 400, billing.CodeUnknownField, "unknown field credits_spec", "credits_spec"},
		{decode(`{"price":"7"}`), 400, api.CodeInvalidParam, "price is invalid", "price"},
		{decode(``), 400, api.CodeInvalidParam, "empty_request_body", ""},
		{decode(`{"key":`), 400, api.CodeInvalidParam, "invalid_request", ""},
		{&http.MaxBytesError{Limit: 1}, 413, billing.CodeRequestBodyTooLarge, "request body too large", ""},
	} {
		got := httprequest.BindError(tc.err)
		param := ""
		if got.Param != nil {
			param = *got.Param
		}
		require.Equal(t, []any{tc.status, api.ErrorTypeInvalidRequest, tc.code, tc.message, tc.param},
			[]any{got.HTTPStatus, got.Type, got.Code, got.Message, param})
	}
}

// Card and checkout refusals render one envelope on every payment surface:
// a customer-safe message, a stable decline_reason and never processor text.
func TestPaymentRefusalEnvelope(t *testing.T) {
	const raw = "raw processor detail that must stay server-side"
	declined := "Your card was declined. Contact your bank or try a different card."
	surfaces := map[string]func(*httprequest.Request, error){
		"checkout": func(r *httprequest.Request, err error) {
			writeCheckoutAttemptError(r, err, checkoutAttemptErrorContext{})
		},
		"tier": writeChangeTierError,
	}
	for _, tc := range []struct {
		err                error
		status             int
		typ, code, message string
		reason, failure    any
	}{
		{&paymentmethods.PaymentMethodError{LocalizationID: "do_not_honor", Message: raw}, 402, "card_error", "card_declined", declined, "do_not_honor", "do_not_honor"},
		{&paymentmethods.PaymentMethodError{LocalizationID: "225", Message: raw}, 402, "card_error", "card_declined", "The card's security code is incorrect.", "incorrect_cvc", "225"},
		{&paymentmethods.PaymentMethodError{LocalizationID: "fraudulent_card", Message: raw}, 402, "card_error", "card_declined", declined, "fraudulent", "fraudulent_card"},
		{&paymentmethods.PaymentMethodError{LocalizationID: "unmapped_gateway_response", Message: raw}, 402, "card_error", "card_declined", declined, "unknown", "unmapped_gateway_response"},
		{&paymentmethods.PaymentMethodError{LocalizationID: "300", Message: raw}, 502, "api_error", "payment_provider_rejected", "The payment processor could not complete this payment. Please try again later.", "gateway_rejected", "300"},
		{fmt.Errorf("%w: %s", checkout.ErrPaymentMethodStale, raw), 402, "card_error", "payment_method_stale", "This saved payment method can no longer be used. Add the card again.", nil, nil},
	} {
		for name, write := range surfaces {
			got := render(t, func(r *httprequest.Request) { write(r, tc.err) })
			require.Equal(t, []any{tc.status, tc.typ, tc.code, tc.message, tc.reason, tc.failure, "req-envelope"},
				[]any{got.Status, got.Type, got.Code, got.Message, got.Metadata["decline_reason"], got.Metadata["failure_code"], got.RequestID}, "%s: %s", name, got.Raw)
			require.NotContains(t, got.Raw, raw)
		}
	}

	checkoutStatus := map[error]int{
		fmt.Errorf("sale: %w", checkout.ErrCheckoutProcessing): 409,
		checkout.ErrCheckoutAttemptPending:                     409,
		checkout.ErrCheckoutAttemptConflict:                    409,
		checkout.ErrCheckoutAttemptNotFound:                    404,
		checkout.ErrCheckoutAttemptForbidden:                   403,
		checkout.ErrCheckoutAttemptExpired:                     410,
		checkout.ErrCheckoutAttemptValidation:                  400,
		checkout.ErrCheckoutCaptureUnavailable:                 503,
		fmt.Errorf("x: %w", billing.ErrIdempotencyKeyReused):   409,
	}
	for err, status := range checkoutStatus {
		got := render(t, func(r *httprequest.Request) { writeCheckoutAttemptError(r, err, checkoutAttemptErrorContext{}) })
		require.Equal(t, status, got.Status, "%v", err)
	}
	blocked := render(t, func(r *httprequest.Request) {
		writeCheckoutAttemptError(r, &checkout.CardAttemptsBlockedError{RetryAfter: 90 * time.Second}, checkoutAttemptErrorContext{})
	})
	require.Equal(t, []any{429, "card_attempts_blocked"}, []any{blocked.Status, blocked.Code})

	tierStatus := map[error]int{
		fmt.Errorf("upgrade: %w", checkout.ErrCheckoutProcessing): 409,
		checkout.ErrTierChangePending:                             409,
		checkout.ErrTierChangeSameProduct:                         409,
		checkout.ErrTierChangeNoSubscription:                      404,
		checkout.ErrTierChangeDifferentGroup:                      400,
		subscriptions.ErrRepriceCrossCurrency:                     400,
	}
	for err, status := range tierStatus {
		require.Equal(t, status, render(t, func(r *httprequest.Request) { writeChangeTierError(r, err) }).Status, "%v", err)
	}
}

func TestTierChangeOutcomeEnvelope(t *testing.T) {
	op := uuid.New()
	got := render(t, func(r *httprequest.Request) {
		writeChangeTierError(r, &checkout.TierChangeInFlightError{OperationID: op})
	})
	require.Equal(t, []any{409, billing.CodeTierChangeInFlight, op.String()}, []any{got.Status, got.Code, got.Metadata["operation_id"]})

	got = render(t, func(r *httprequest.Request) {
		writeChangeTierError(r, &checkout.TierChangeDeclinedError{Rail: "stripe", FailureCode: "insufficient_funds", Message: "raw provider text"})
	})
	require.Equal(t, []any{402, billing.CodeCardDeclined, "card_error", "insufficient_funds"}, []any{got.Status, got.Code, got.Type, got.Metadata["decline_reason"]})
	require.NotContains(t, got.Raw, "raw provider text")

	// A tier change refusal's code fixes its status.
	for code, status := range map[string]int{
		billing.CodeTierChangeRefused:           409,
		billing.CodeTierChangeTargetInactive:    422,
		billing.CodeTierChangeUnsupportedOnRail: 400,
		billing.CodeTierChangeProviderConflict:  409,
		billing.CodeCustomerActionRequired:      403,
	} {
		got = render(t, func(r *httprequest.Request) {
			writeChangeTierError(r, &checkout.TierChangeError{Code: code, Message: "x"})
		})
		require.Equal(t, []any{status, code}, []any{got.Status, got.Code})
	}

	for status, want := range map[string]int{"processing": http.StatusAccepted, "succeeded": http.StatusOK} {
		r, rec := newTestRequest(http.MethodPost, "/", nil, &app.Runtime{})
		writeTierChangeResponse(r, &checkout.TierChangeResponse{Status: status, OperationID: billing.PaymentOperationID{1}})
		require.Equal(t, want, rec.Code)
		var body checkout.TierChangeResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Equal(t, billing.PaymentOperationID{1}, body.OperationID, "a lost first response can be read back")
	}
}

func TestInsufficientUSDCCarriesFundingMetadata(t *testing.T) {
	got := render(t, func(r *httprequest.Request) {
		writeCheckoutAttemptError(r, &recurring.InsufficientUSDCError{HaveBaseUnits: 250_000, NeedBaseUnits: 1_500_000},
			checkoutAttemptErrorContext{Rail: "solana", Wallet: "11111111111111111111111111111111", CheckoutAttemptID: "chk_123"})
	})
	require.Equal(t, []any{402, "insufficient_funds", "usdc_balance"}, []any{got.Status, got.Code, got.param()})
	require.Equal(t, map[string]any{
		"asset": "USDC", "network": "solana", "rail": "solana",
		"wallet": "11111111111111111111111111111111", "checkout_attempt_id": "chk_123",
		"amount": "1.5", "balance": "0.25", "shortfall": "1.25",
		"amount_base_units": "1500000", "balance_base_units": "250000", "shortfall_base_units": "1250000",
	}, got.Metadata["usdc_funding"])
}

func TestProviderTransportErrorsAreNotEchoed(t *testing.T) {
	ambiguous := createPaymentMethodProviderError(fmt.Errorf("create vault: %w", &nmi.TransportAmbiguousError{Err: errors.New("timed out after send")}))
	require.NotNil(t, ambiguous)
	require.Equal(t, []any{409, codePaymentMethodProviderOutcomeUnknown}, []any{ambiguous.HTTPStatus, ambiguous.Code})
	require.Contains(t, ambiguous.Message, "Refresh your payment methods")
	require.Nil(t, createPaymentMethodProviderError(errors.New("declined")))

	// SEC-17: a real RPC failure never reaches the customer with the credentialed URL.
	const secret = "merchant-rpc-secret-key"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	client := solanarpc.NewRPCClientWithConfig(solanarpc.RPCClientConfig{Endpoint: srv.URL + "/?api-key=" + secret, Network: "mainnet"})
	_, err := client.GetBalance(context.Background(), solanago.MustPublicKeyFromBase58("11111111111111111111111111111111"))
	require.Error(t, err)
	refusal := solanaClientError(err)
	require.Equal(t, []any{http.StatusBadGateway, codeSolanaRPCUnavailable, "Solana RPC is temporarily unavailable; please retry"},
		[]any{refusal.HTTPStatus, refusal.Code, refusal.Message})

	refusal = solanaClientError(errors.New("subscriber already enrolled"))
	require.Equal(t, []any{http.StatusBadRequest, codeSolanaTransactionRefused, "subscriber already enrolled"},
		[]any{refusal.HTTPStatus, refusal.Code, refusal.Message})
}
