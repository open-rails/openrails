//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

func TestMerchantCardRemoval(t *testing.T) {
	t.Run("OR-J12-S07/merchant-card-removal-keeps-customer-scope", func(t *testing.T) {
		a, journal := newNMIWorld(t)
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)
		// NMI's v5 refusals, as its sandbox answers them: a request it finds
		// invalid is the provider's refusal, a card it will not take is the
		// customer's decline. Neither shows NMI's own code or text.
		for _, refusal := range []struct {
			envelope string
			code     billing.ErrorCode
			recorded string
			status   int
		}{
			{`{"type":"validationError","error_code":"E_INVALID_SUBMISSION","message":"The provided data is invalid."}`, billing.CodePaymentProviderRejected, "e_invalid_submission", http.StatusBadGateway},
			{`{"type":"generalError","error_code":"E_GENERIC_ERROR","message":"Bad Request. Please use a valid test card or test ACH account from the documentation."}`, billing.CodeCardDeclined, "e_generic_error", http.StatusPaymentRequired},
		} {
			journal.core.Intercept(func(r *http.Request) bool {
				return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v5/customers")
			},
				func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
					return answer(r, http.StatusBadRequest, refusal.envelope), nil
				})
			status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, billing.CreatePaymentMethodParams{PSPID: psps.Items[0].ID, Token: journal.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242", Exp: "1235"}), BillingDetails: billedTo("US", "10001")})
			journal.core.ClearIntercepts()
			require.Equal(t, refusal.status, status, string(raw))
			require.Contains(t, string(raw), refusal.code)
			require.NotContains(t, strings.ToLower(string(raw)), refusal.recorded)
			require.NotContains(t, string(raw), "test card")
			tried, err := a.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{ResponseCode: []string{refusal.recorded}})
			require.NoError(t, err)
			require.Len(t, tried.Items, 1, "the attempt records NMI's error code")
		}
		status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, billing.CreatePaymentMethodParams{PSPID: psps.Items[0].ID, Token: journal.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242", Exp: "1235"}), BillingDetails: billedTo("US", "10001")})
		require.Equal(t, 201, status, string(raw))
		var method billing.PaymentMethod
		require.NoError(t, json.Unmarshal(raw, &method))
		reader, err := a.auth.CreateAPIKey(t.Context(), iam.SystemIdentity(), a.alphaGroup, iam.NewAPIKey{Name: "card-reader", Role: a.reader})
		require.NoError(t, err)
		path := "/v1/admin/customers/" + method.CustomerID.String() + "/payment-methods/" + method.ID.String()
		for _, check := range []struct {
			token  string
			status int
		}{{"", 401}, {a.bobToken, 403}, {reader.Secret, 403}, {a.betaKey.Secret, 404}} {
			status, raw = a.request(t, http.MethodDelete, path, check.token, nil)
			require.Equal(t, check.status, status, string(raw))
		}
		status, raw = a.request(t, http.MethodDelete, "/v1/admin/customers/"+billing.CustomerID(uuid.MustParse(a.bob.ID)).String()+"/payment-methods/"+method.ID.String(), a.alphaKey.Secret, nil)
		require.Equal(t, 404, status, string(raw))
		kept, err := a.alpha.ListPaymentMethods(t.Context(), method.CustomerID, billing.PaymentMethodListParams{})
		require.NoError(t, err)
		require.Len(t, kept.Items, 1)
		setWritePosture(t, a.world, "readonly", a.alpha.MerchantID())
		require.Error(t, a.alpha.DeletePaymentMethod(t.Context(), method.CustomerID, method.ID), "an explicit host request never bypasses the merchant's readonly write posture")
		require.Len(t, journal.core.Vaults(), 1, "read-only removal sends no provider mutation")
		setWritePosture(t, a.world, "full", a.alpha.MerchantID())
		status, raw = a.request(t, http.MethodDelete, path, a.alphaKey.Secret, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		require.NoError(t, a.alphaRemote.DeletePaymentMethod(t.Context(), method.CustomerID, method.ID))
		require.NoError(t, a.alpha.DeletePaymentMethod(t.Context(), method.CustomerID, method.ID), "embedded host uses the same confirmed-operation replay")
		after, err := a.alpha.ListPaymentMethods(t.Context(), method.CustomerID, billing.PaymentMethodListParams{})
		require.NoError(t, err)
		require.Empty(t, after.Items, "removed cards are no longer offered for charging")
		require.Empty(t, journal.core.Vaults(), "the external holder confirmed removal")
		journal.money(t, 0, 0)
	})

	t.Run("OR-J12-S71/nmi-erasure-requires-confirmed-absence", func(t *testing.T) {
		a, journal := newNMIWorld(t)
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)

		for _, failReadBeforeDelete := range []bool{true, false} {
			status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, billing.CreatePaymentMethodParams{
				PSPID:          psps.Items[0].ID,
				Token:          journal.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242", Exp: "1235"}),
				BillingDetails: billedTo("US", "10001"),
			})
			require.Equal(t, http.StatusCreated, status, string(raw))
			var method billing.PaymentMethod
			require.NoError(t, json.Unmarshal(raw, &method))
			vaults := journal.core.Vaults()
			require.Len(t, vaults, 1)
			recordPath := "/v5/customers/" + vaults[0].ID
			matchesVault := func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, recordPath) }
			var deletes atomic.Int32
			journal.core.Intercept(matchesVault, func(r *http.Request, next func() *http.Response) (*http.Response, error) {
				if r.Method == http.MethodDelete && !failReadBeforeDelete {
					deletes.Add(1)
					response := next()
					_ = response.Body.Close()
					return answer(r, http.StatusNotFound, "route not found"), nil
				}
				if r.Method == http.MethodGet && (failReadBeforeDelete || deletes.Load() > 0) {
					return answer(r, http.StatusNotFound, "route not found"), nil
				}
				return next(), nil
			})

			path := "/v1/admin/customers/" + method.CustomerID.String() + "/payment-methods/" + method.ID.String()
			status, raw = a.request(t, http.MethodDelete, path, a.alphaKey.Secret, nil)
			require.Equal(t, http.StatusServiceUnavailable, status, string(raw))
			var operationStatus, parkReason string
			intents := pgx.Identifier{a.schema, "provider_intents"}.Sanitize()
			methods := pgx.Identifier{a.schema, "payment_methods"}.Sanitize()
			err = a.pool.QueryRow(t.Context(), `SELECT i.status, m.park_reason FROM `+intents+` i JOIN `+methods+` m ON m.id::text = i.payload->>'payment_method_id' AND m.merchant_id = i.merchant_id WHERE i.intent_type = 'nmi_vault_delete' AND m.id = $1`, method.ID.UUID()).Scan(&operationStatus, &parkReason)
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(parkReason, "delete:"), "an unresolved erasure retains its fence")
			if failReadBeforeDelete {
				require.Equal(t, "failed_retryable", operationStatus)
				require.Len(t, journal.core.Vaults(), 1, "an inconclusive preflight sends no delete")
			} else {
				require.Equal(t, "unknown_needs_verify", operationStatus)
				require.Empty(t, journal.core.Vaults(), "the ambiguous response followed an actual provider deletion")
				require.EqualValues(t, 1, deletes.Load())
			}

			journal.core.ClearIntercepts()
			if !failReadBeforeDelete {
				journal.core.Intercept(matchesVault, func(r *http.Request, next func() *http.Response) (*http.Response, error) {
					if r.Method == http.MethodGet {
						return answer(r, http.StatusNotFound, `{"error_code":"E_NOT_FOUND"}`), nil
					}
					deletes.Add(1)
					return next(), nil
				})
			}
			a.clock.Advance(time.Hour)
			status, raw = a.request(t, http.MethodDelete, path, a.alphaKey.Secret, nil)
			require.Equal(t, http.StatusNoContent, status, string(raw))
			journal.core.ClearIntercepts()
			err = a.pool.QueryRow(t.Context(), `SELECT status FROM `+intents+` WHERE intent_type = 'nmi_vault_delete' AND payload->>'payment_method_id' = $1`, method.ID.UUID().String()).Scan(&operationStatus)
			require.NoError(t, err)
			require.Equal(t, "succeeded", operationStatus)
			require.Empty(t, journal.core.Vaults())
			if !failReadBeforeDelete {
				require.EqualValues(t, 1, deletes.Load(), "verified absence completes without sending another delete")
			}
		}
	})
	t.Run("OR-J12-S72/a-card-with-history-is-never-hard-deleted", func(t *testing.T) {
		a, journal := newNMIWorld(t)
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)
		status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, billing.CreatePaymentMethodParams{PSPID: psps.Items[0].ID, Token: journal.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242", Exp: "1235"}), BillingDetails: billedTo("US", "10001")})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var method billing.PaymentMethod
		require.NoError(t, json.Unmarshal(raw, &method))
		versions := func() int {
			t.Helper()
			var n int
			require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema, "payment_method_versions"}.Sanitize()+" WHERE merchant_id = $1 AND payment_method_id = $2",
				a.alpha.MerchantID().UUID(), method.ID.UUID()).Scan(&n))
			return n
		}
		require.Equal(t, 1, versions(), "a saved card's history starts with it")
		_, err = a.pool.Exec(t.Context(), "DELETE FROM "+pgx.Identifier{a.schema, "payment_methods"}.Sanitize()+" WHERE merchant_id = $1 AND id = $2", a.alpha.MerchantID().UUID(), method.ID.UUID())
		var refused *pgconn.PgError
		require.ErrorAs(t, err, &refused, "a card's permanent versions refuse its deletion")
		require.Equal(t, "23503", refused.Code)
		require.Equal(t, 1, versions())
	})

	t.Run("OR-J12-S73/a-card-an-unverified-membership-bills-is-kept", func(t *testing.T) {
		a, journal, sub := nmiMembership(t)
		// The host's book declares the membership canceled while the switch is
		// off: it waits unverified, still on its card.
		asOf := sub.CurrentPeriodStartsAt.Add(time.Hour)
		_, err := a.alpha.ImportBilling(t.Context(), billing.DeclaredBilling{AsOf: asOf, DefaultPSP: billing.PSPRef{Key: "nmi"}, Customers: []billing.DeclaredCustomer{{Customer: sub.CustomerID}},
			Subscriptions: []billing.DeclaredSubscription{{SourceID: "known-nmi-membership", Customer: sub.CustomerID, Price: sub.PriceID, Rail: "nmi", RailSubscriptionID: *sub.RailSubscriptionID,
				StartedAt: *sub.CurrentPeriodStartsAt, PaidThrough: sub.CurrentPeriodEndsAt, Cancel: billing.CancelEvidence{Kind: "user_canceled", At: asOf.Add(-time.Minute)}}}})
		require.NoError(t, err)
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionUnverified, current.Status)
		methods, err := a.alpha.ListPaymentMethods(t.Context(), sub.CustomerID, billing.PaymentMethodListParams{})
		require.NoError(t, err)
		require.Len(t, methods.Items, 1)
		status, raw := a.request(t, http.MethodDelete, "/v1/me/payment-methods/"+methods.Items[0].ID.String(), a.aliceToken, nil)
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), "add a new card first", "the customer is told what to do")
		require.Len(t, journal.core.Vaults(), 1, "the schedule's vault stays")
	})
}
