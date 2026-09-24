package idguard

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/merchant"
)

// A zero id is a supplied value, never "unset": it is a coded 400 naming the
// field. Only a nil pointer is genuinely absent.
func TestGuardsRefuseZeroIdentifiers(t *testing.T) {
	set, zero := uuid.New(), uuid.Nil
	for _, tc := range []struct {
		name, param string
		err         error
	}{
		{"required zero", "subscription_id", Require("subscription_id", zero)},
		{"required set", "", Require("subscription_id", set)},
		{"optional nil", "", RequireOptional("target_psp_id", nil)},
		{"optional zero", "target_psp_id", RequireOptional("target_psp_id", &zero)},
		{"optional set", "", RequireOptional("target_psp_id", &set)},
		{"merchant zero", "merchant_id", RequireMerchant("merchant_id", merchant.ID{})},
		{"merchant set", "", RequireMerchant("merchant_id", merchant.ID(set))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.param == "" {
				require.NoError(t, tc.err)
				return
			}
			require.ErrorIs(t, tc.err, openrails.ErrInvalid)
			var se *apperr.Error
			require.ErrorAs(t, tc.err, &se)
			require.Equal(t, 400, se.Status)
			require.Equal(t, "invalid_param", se.Code)
			require.Equal(t, tc.param, se.Param)
			require.Contains(t, se.Message, tc.param)
		})
	}
}

// Wire spellings: blank parses to the zero id (then refused above); an explicit
// zero UUID is refused at parse (#479).
func TestWireIdentifierSpellingsNeverYieldAUsableZero(t *testing.T) {
	for prefix, parse := range map[string]func(string) (bool, error){
		"sub_": func(s string) (bool, error) { id, err := openrails.ParseSubscriptionID(s); return id.IsZero(), err },
		"pm_":  func(s string) (bool, error) { id, err := openrails.ParsePaymentMethodID(s); return id.IsZero(), err },
	} {
		for _, blank := range []string{"", " ", "\t  \n"} {
			isZero, err := parse(blank)
			require.NoError(t, err)
			require.True(t, isZero)
		}
		_, err := parse(prefix + uuid.Nil.String())
		require.Error(t, err, prefix)
	}
}
