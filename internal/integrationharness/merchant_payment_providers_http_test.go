//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
)

func TestStandaloneMerchantPaymentProviderConfigHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	adminToken := surface.MintAPIKey(
		dbtest.TestMerchantSlug,
		"provider-admin-"+uuid.NewString(),
		[]string{controlplane.PermMerchantPaymentProvidersRead, controlplane.PermMerchantPaymentProvidersUpdate},
	)
	readToken := surface.MintAPIKey(
		dbtest.TestMerchantSlug,
		"provider-reader-"+uuid.NewString(),
		[]string{controlplane.PermMerchantPaymentProvidersRead},
	)
	// #567: API keys are role-based. A customer-settings:read key maps to the
	// `support` role, which has NO payment-providers perm — the faithful "lacks
	// payment-provider access" principal under the fixed catalog roles (there is
	// no role holding catalog:read in isolation; catalog:read would widen to
	// viewer, which does carry payment-providers:read).
	deniedToken := surface.MintAPIKey(
		dbtest.TestMerchantSlug,
		"provider-denied-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCustomerSettingsRead},
	)

	accountID := "acct_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	status, body := requestJSON(t, http.MethodPut, surface.BaseURL+"/v1/merchant/payment-providers/stripe", adminToken, map[string]any{
		"environment": "live",
		"account_id":  accountID,
		"public_config": map[string]string{
			"publishable_key": "pk_test_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		},
		"credentials": map[string]string{
			"webhook_signing_secret": "whsec_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		},
	})
	require.Equal(t, http.StatusOK, status, string(body))
	require.NotContains(t, string(body), "whsec_")
	var putResp struct {
		PaymentProvider merchants.PaymentProviderConfig `json:"payment_provider"`
	}
	require.NoError(t, json.Unmarshal(body, &putResp))
	require.Equal(t, "stripe", putResp.PaymentProvider.ProviderType)
	require.Equal(t, accountID, putResp.PaymentProvider.AccountID)
	require.True(t, putResp.PaymentProvider.Credentials["webhook_signing_secret"].Configured)

	deniedReadStatus, deniedReadBody := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/payment-providers", deniedToken, nil)
	require.Equal(t, http.StatusForbidden, deniedReadStatus, string(deniedReadBody))

	deniedWriteStatus, deniedWriteBody := requestJSON(t, http.MethodPut, surface.BaseURL+"/v1/merchant/payment-providers/stripe", readToken, map[string]any{
		"environment": "live",
		"account_id":  accountID,
	})
	require.Equal(t, http.StatusForbidden, deniedWriteStatus, string(deniedWriteBody))

	getStatus, getBody := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/payment-providers/stripe?environment=live", readToken, nil)
	require.Equal(t, http.StatusOK, getStatus, string(getBody))
	require.NotContains(t, string(getBody), "whsec_")
	var getResp struct {
		PaymentProvider merchants.PaymentProviderConfig `json:"payment_provider"`
	}
	require.NoError(t, json.Unmarshal(getBody, &getResp))
	require.Equal(t, accountID, getResp.PaymentProvider.AccountID)

	invalidAccountID := "nmi_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	invalidStatus, invalidBody := requestJSON(t, http.MethodPut, surface.BaseURL+"/v1/merchant/payment-providers/nmi", adminToken, map[string]any{
		"environment": "live",
		"account_id":  invalidAccountID,
		"credentials": map[string]string{
			"tokenization_url": "http://example.test/collect.js",
		},
	})
	require.Equal(t, http.StatusBadRequest, invalidStatus, string(invalidBody))

	listStatus, listBody := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/payment-providers?provider=nmi&environment=live", readToken, nil)
	require.Equal(t, http.StatusOK, listStatus, string(listBody))
	require.NotContains(t, string(listBody), invalidAccountID)

	deleteStatus, deleteBody := requestJSON(t, http.MethodDelete, surface.BaseURL+"/v1/merchant/payment-providers/stripe?environment=live", adminToken, nil)
	require.Equal(t, http.StatusOK, deleteStatus, string(deleteBody))
	var deleteResp struct {
		PaymentProvider merchants.PaymentProviderConfig `json:"payment_provider"`
	}
	require.NoError(t, json.Unmarshal(deleteBody, &deleteResp))
	require.Equal(t, "disabled", deleteResp.PaymentProvider.Status)

	afterDeleteStatus, afterDeleteBody := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/payment-providers/stripe?environment=live", readToken, nil)
	require.Equal(t, http.StatusNotFound, afterDeleteStatus, string(afterDeleteBody))
}
