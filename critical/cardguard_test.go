//go:build integration

package critical_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/stretchr/testify/require"
)

func TestCardGuard(t *testing.T) {
	t.Run("OR-J12-S12/mastercard-length-rule-keeps-real-cards-blocked", func(t *testing.T) {
		for _, tc := range []struct {
			name, value string
			blocked     bool
		}{
			{"mastercard-51", "5100000000000008", true},
			{"mastercard-52", "5200000000000007", true},
			{"mastercard-53", "5300000000000006", true},
			{"mastercard-54", "5400000000000005", true},
			{"mastercard-55", "5500000000000004", true},
			{"reference-51", "510000000000000008", false},
			{"reference-52", "520000000000000007", false},
			{"reference-53", "530000000000000006", false},
			{"reference-54", "540000000000000005", false},
			{"reference-55", "550000000000000004", false},
			{"mastercard-grouped", "5100 0000 0000 0008", true},
			{"mastercard-dashes", "5200-0000-0000-0007", true},
			{"mastercard-in-text", "reference=5300000000000006", true},
			{"bad-check-digit", "5100000000000009", false},
			{"maestro-50", "500000000000000009", true},
			{"maestro-56", "560000000000000003", true},
			{"visa", "4111111111111111", true},
			{"amex", "378282246310005", true},
			{"mastercard-two-series", "2223000048400011", true},
			{"discover", "6011111111111117", true},
			{"jcb", "3530111333300000", true},
			{"uuid", "550e8400-e29b-41d4-a716-446655440000", false},
		} {
			t.Run(tc.name, func(t *testing.T) { require.Equal(t, tc.blocked, cardguard.ContainsPAN(tc.value)) })
		}
	})
	t.Run("OR-J38-S36/ccbill-references-import-and-export-unchanged", func(t *testing.T) {
		iw := bootImportWorld(t, "ccbill")
		product, err := iw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "legacy-ccbill", DisplayName: "Legacy CCBill", Entitlements: []string{"legacy:member"}})
		require.NoError(t, err)
		price, err := iw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "legacy-monthly", PSP: "ccbill", FlexID: "681cb38f-afb9-4665-931f-2b896072178a", RecurringBillingOptionID: "0000000023", UnitAmount: ptr(int64(23_000_000)), Currency: "USD", BillingIntervalHours: ptr(720), Archived: true})
		require.NoError(t, err)
		customer := billing.CustomerID(uuid.MustParse(iw.alice.ID))
		started, ended := iw.clock.Now().Add(-60*24*time.Hour), iw.clock.Now().Add(-30*24*time.Hour)
		book := billing.DeclaredBilling{AsOf: iw.clock.Now(), DefaultPSP: billing.PSPRef{Key: "ccbill"}, Customers: []billing.DeclaredCustomer{{Customer: customer}}}
		references := []string{"510000000000000008", "520000000000000007"}
		for i, ref := range references {
			book.Subscriptions = append(book.Subscriptions, billing.DeclaredSubscription{SourceID: fmt.Sprintf("ccbill-%d", i), Customer: customer, Price: price.ID, Rail: "ccbill", RailSubscriptionID: ref, StartedAt: started, PaidThrough: &ended, Cancel: billing.CancelEvidence{Kind: "provider_terminated", At: ended}, Evidence: json.RawMessage(fmt.Sprintf(`{"ccbill_sub_id":%s,"provider_reference":"%s"}`, ref, ref))})
		}
		result, err := iw.alphaRemote.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"ccbill-0", "ccbill-1"}, result.Imported)
		require.Empty(t, result.Blocked)
		result, err = iw.alphaRemote.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Empty(t, result.Imported)
		require.ElementsMatch(t, []string{"ccbill-0", "ccbill-1"}, result.Skipped)
		subscriptions, err := iw.alphaRemote.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, subscriptions.Items, 2)
		found := make([]string, 0, 2)
		for _, sub := range subscriptions.Items {
			require.NotNil(t, sub.RailSubscriptionID)
			found = append(found, *sub.RailSubscriptionID)
			var raw []byte
			err := iw.pool.QueryRow(t.Context(), "SELECT gateway_response FROM "+pgx.Identifier{iw.schema, "subscriptions"}.Sanitize()+" WHERE merchant_id=$1 AND id=$2", iw.alpha.MerchantID().UUID(), sub.ID.UUID()).Scan(&raw)
			require.NoError(t, err)
			var evidence struct {
				CCBillSubID json.Number `json:"ccbill_sub_id"`
				Reference   string      `json:"provider_reference"`
			}
			require.NoError(t, json.Unmarshal(raw, &evidence))
			require.Equal(t, *sub.RailSubscriptionID, evidence.CCBillSubID.String())
			require.Equal(t, *sub.RailSubscriptionID, evidence.Reference)
		}
		require.ElementsMatch(t, references, found)
		require.NotEmpty(t, archiveBytes(t, iw.alphaRemote), "archive validation must accept the same provider references")
		for _, field := range []string{"typed-reference", "evidence", "escaped-evidence"} {
			t.Run(field, func(t *testing.T) {
				refused := book
				refused.Subscriptions = []billing.DeclaredSubscription{book.Subscriptions[0]}
				refused.Subscriptions[0].SourceID = "refused-" + field
				refused.Subscriptions[0].RailSubscriptionID = "not-a-card"
				refused.Subscriptions[0].Evidence = nil
				switch field {
				case "typed-reference":
					refused.Subscriptions[0].RailSubscriptionID = "5100000000000008"
				case "evidence":
					refused.Subscriptions[0].Evidence = json.RawMessage(`{"reference":"5200-0000-0000-0007"}`)
				case "escaped-evidence":
					refused.Subscriptions[0].Evidence = json.RawMessage(`{"reference":"\u0035\u0033\u0030\u0030\u0030\u0030\u0030\u0030\u0030\u0030\u0030\u0030\u0030\u0030\u0030\u0036"}`)
				}
				_, err := iw.alphaRemote.ImportBilling(t.Context(), refused)
				require.Error(t, err)
				var apiErr *billing.StatusError
				require.ErrorAs(t, err, &apiErr)
				require.Equal(t, http.StatusBadRequest, apiErr.Status)
				require.Equal(t, billing.CodeInvalidParam, apiErr.Code)
				require.Contains(t, apiErr.Message, "card-number-shaped")
				require.False(t, strings.Contains(apiErr.Message, "5100000000000008"))
				after, err := iw.alphaRemote.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
				require.NoError(t, err)
				require.Len(t, after.Items, 2, "a refused book must not write a subscription")
			})
		}
	})
}
