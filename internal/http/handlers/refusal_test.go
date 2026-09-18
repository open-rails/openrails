package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/billingimport"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	billingservice "github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/api"
)

type observedRefusal struct {
	Status int
	Code   string
	Param  string
}

func observeRefusal(t *testing.T, write func(*httprequest.Request, error), err error) (observedRefusal, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httprequest.NewHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil), nil)
	write(r, err)
	var body api.ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	out := observedRefusal{Status: rec.Code, Code: body.Error.Code}
	if body.Error.Param != nil {
		out.Param = *body.Error.Param
	}
	return out, body.Error.Message
}

// reworded returns the same refusal with a different human message: the
// #983 acceptance is that this changes nothing a caller can branch on.
func reworded(err error) error {
	var refusal *apperr.Error
	if !errors.As(err, &refusal) {
		return errors.New("some other wording entirely")
	}
	return fmt.Errorf("outer context changed too: %w", &apperr.Error{
		Status: refusal.Status, Code: refusal.Code, Param: refusal.Param,
		Message: "REWORDED " + refusal.Message + " (edited by a later PR)",
	})
}

// TestRefusalClassificationIgnoresHumanMessage drives every message-classified
// site through its handler mapper twice — once with the real error and once
// with the same refusal reworded — and asserts status, code and param never
// move. Untyped errors are internal failures whose text never reaches the
// client.
func TestRefusalClassificationIgnoresHumanMessage(t *testing.T) {
	writeCatalog := func(r *httprequest.Request, err error) { writeCatalogError(r, err) }
	writeCancel := func(r *httprequest.Request, err error) { writeRefusal(r, err, "failed to cancel subscription") }
	writeReprice := func(r *httprequest.Request, err error) { writeRepriceError(r, err) }
	writeImport := func(r *httprequest.Request, err error) { writeRefusal(r, err, "billing import failed") }
	writeProvider := func(r *httprequest.Request, err error) { writeMerchantProviderError(r, err) }
	writeDeposit := func(r *httprequest.Request, err error) { writeRefusal(r, err, "deposit failed") }

	cases := []struct {
		name  string
		write func(*httprequest.Request, error)
		err   error
		want  observedRefusal
	}{
		{"catalog product not found", writeCatalog, billingservice.ErrProductNotFound, observedRefusal{404, "product_not_found", ""}},
		{"catalog price not found", writeCatalog, billingservice.ErrPriceNotFound, observedRefusal{404, "price_not_found", ""}},
		{"catalog price key not found", writeCatalog, billingservice.ErrPriceKeyNotFound, observedRefusal{404, "price_key_not_found", ""}},
		{"catalog unique violation", writeCatalog, billingservice.ErrCatalogConflict, observedRefusal{409, api.CodeResourceConflict, ""}},
		{"catalog tier group in use", writeCatalog, billingservice.ErrProductTierGroupInUse, observedRefusal{409, "product_tier_group_in_use", ""}},
		{"catalog trial unsupported", writeCatalog, fmt.Errorf("%w: price declares a trial", billingservice.ErrTrialUnsupportedOnRail), observedRefusal{400, "trial_unsupported_on_rail", ""}},
		{"catalog validation", writeCatalog, apperr.Invalidf("unit_amount must be non-negative"), observedRefusal{400, api.CodeInvalidParam, ""}},
		{"catalog currency", writeCatalog, billingservice.ErrCurrencyUnsupported.WithParam("currency"), observedRefusal{400, "currency_unsupported", "currency"}},
		{"cancel subscription not found", writeCancel, subscriptions.ErrSubscriptionNotFound, observedRefusal{404, "subscription_not_found", ""}},
		{"cancel subscription not active", writeCancel, subscriptions.ErrSubscriptionNotActive, observedRefusal{409, "subscription_not_active", ""}},
		{"cancel unsupported rail", writeCancel, fmt.Errorf("%w: %s", subscriptions.ErrCancelUnsupportedOnRail, "carrier_pigeon"), observedRefusal{400, "cancel_unsupported_on_rail", ""}},
		{"cancel solana needs signature", writeCancel, subscriptions.ErrSolanaCancelNeedsWalletSignature, observedRefusal{400, "solana_cancel_needs_wallet_signature", ""}},
		{"reprice subscription not found", writeReprice, subscriptions.ErrSubscriptionNotFound, observedRefusal{404, "subscription_not_found", ""}},
		{"reprice target price not found", writeReprice, subscriptions.ErrRepriceTargetPriceNotFound, observedRefusal{404, "reprice_target_price_not_found", ""}},
		{"reprice price key not found", writeReprice, fmt.Errorf("%w: price key %q has no current price", subscriptions.ErrRepricePriceKeyNotFound, "k"), observedRefusal{404, "reprice_price_key_not_found", ""}},
		{"reprice not found", writeReprice, subscriptions.ErrRepriceNotFound, observedRefusal{404, "reprice_not_found", ""}},
		{"reprice constraint", writeReprice, &subscriptions.RepriceConstraintError{Sentinel: subscriptions.ErrRepriceCrossProduct}, observedRefusal{422, "reprice_cross_product", ""}},
		{"reprice notice window", writeReprice, &subscriptions.RepriceConstraintError{Sentinel: subscriptions.ErrRepriceNoticeWindowViolation}, observedRefusal{422, "reprice_notice_window_violation", ""}},
		{"reprice already scheduled", writeReprice, &subscriptions.RepriceConstraintError{Sentinel: subscriptions.ErrRepriceAlreadyScheduled}, observedRefusal{409, "reprice_already_scheduled", ""}},
		{"reprice not scheduled", writeReprice, subscriptions.ErrRepriceNotScheduled, observedRefusal{409, "reprice_not_scheduled", ""}},
		{"reprice validation", writeReprice, apperr.Invalidf("price_key required"), observedRefusal{400, api.CodeInvalidParam, ""}},
		{"import invalid psp reference", writeImport, fmt.Errorf("%w: row 7 names PSP key %q", billingimport.ErrInvalidPSPReference, "x"), observedRefusal{400, "invalid_psp_reference", ""}},
		{"import card-data refusal", writeImport, fmt.Errorf("%w: declared field refused", billingimport.ErrInvalidDeclaredInput), observedRefusal{400, api.CodeInvalidParam, ""}},
		{"import validation", writeImport, apperr.Invalidf("declared payment method requires rail"), observedRefusal{400, api.CodeInvalidParam, ""}},
		{"import ownership conflict", writeImport, apperr.Conflictf("payment method belongs to another customer"), observedRefusal{409, api.CodeResourceConflict, ""}},
		{"provider validation", writeProvider, apperr.Invalidf("merchants: unsupported payment rail %q", "abacus"), observedRefusal{400, api.CodeInvalidParam, ""}},
		{"provider credentials rejected", writeProvider, fmt.Errorf("%w: stripe key rejected (401)", merchants.ErrPaymentProviderCredentialsRejected), observedRefusal{400, "payment_provider_credentials_rejected", ""}},
		{"provider not configured", writeProvider, merchants.ErrPaymentProviderNotFound, observedRefusal{404, "payment_provider_not_found", ""}},
		{"deposit currency", writeDeposit, billingservice.ErrCurrencyUnsupported.WithParam("currency"), observedRefusal{400, "currency_unsupported", "currency"}},
		{"deposit validation", writeDeposit, apperr.Invalidf("amount must be > 0"), observedRefusal{400, api.CodeInvalidParam, ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := observeRefusal(t, tc.write, tc.err)
			require.Equal(t, tc.want, got, "message=%q", msg)
			mutated, mutatedMsg := observeRefusal(t, tc.write, reworded(tc.err))
			require.NotEqual(t, msg, mutatedMsg, "the mutation must actually change the wire message")
			require.Equal(t, got, mutated, "a changed human message changed the classification")
		})
	}

	t.Run("untyped errors are internal and never leak", func(t *testing.T) {
		for name, write := range map[string]func(*httprequest.Request, error){
			"catalog": writeCatalog, "cancel": writeCancel, "reprice": writeReprice,
			"import": writeImport, "provider": writeProvider, "deposit": writeDeposit,
		} {
			for _, err := range []error{
				errors.New(`ERROR: duplicate key value violates unique constraint "x" (SQLSTATE 23505)`),
				errors.New("product not found"),
				errors.New("subscription is not active"),
				errors.New("account_id required and invalid and unknown"),
				errors.New("money: unknown currency \"XXX\""),
			} {
				got, msg := observeRefusal(t, write, err)
				require.Equal(t, observedRefusal{500, api.CodeInternalError, ""}, got, "%s: %v", name, err)
				require.NotContains(t, msg, err.Error(), "%s leaked the internal error text", name)
			}
		}
	})
}
