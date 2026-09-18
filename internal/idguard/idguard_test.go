package idguard

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Every identifier shape a caller can supply: a zero UUID is refused as an
// invalid parameter, a nil pointer is genuinely absent, and a real id passes.
func TestGuardsRefuseZeroIdentifiers(t *testing.T) {
	set := uuid.New()
	zero := uuid.Nil
	tests := []struct {
		name    string
		err     error
		refused bool
		param   string
	}{
		{"required zero", Require("subscription_id", uuid.Nil), true, "subscription_id"},
		{"required set", Require("subscription_id", set), false, ""},
		{"optional nil pointer", RequireOptional("target_psp_id", nil), false, ""},
		{"optional pointer to zero", RequireOptional("target_psp_id", &zero), true, "target_psp_id"},
		{"optional pointer to id", RequireOptional("target_psp_id", &set), false, ""},
		{"merchant zero", RequireMerchant("merchant_id", merchant.ID{}), true, "merchant_id"},
		{"merchant set", RequireMerchant("merchant_id", merchant.ID(set)), false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.refused {
				require.NoError(t, tt.err)
				return
			}
			require.Error(t, tt.err)
			require.ErrorIs(t, tt.err, openrails.ErrInvalid)
			var se *openrails.StatusError
			require.ErrorAs(t, tt.err, &se)
			require.Equal(t, 400, se.Status)
			require.Equal(t, "invalid_param", se.Code)
			require.NotNil(t, se.Param)
			require.Equal(t, tt.param, *se.Param)
			require.Contains(t, se.Message, tt.param)
		})
	}
}

// The wire spellings of the same identifiers: blank and whitespace parse to the
// zero id (which every entry point then refuses), and an explicit zero UUID is
// refused at parse (#479). Together with the guards above this covers each id
// across {zero UUID, empty string, whitespace}.
func TestWireIdentifierSpellingsNeverYieldAUsableZero(t *testing.T) {
	kinds := map[string]func(string) (bool, error){
		"subscription_id": func(s string) (bool, error) {
			id, err := openrails.ParseSubscriptionID(s)
			return id.IsZero(), err
		},
		"payment_method_id": func(s string) (bool, error) {
			id, err := openrails.ParsePaymentMethodID(s)
			return id.IsZero(), err
		},
	}
	prefixes := map[string]string{"subscription_id": "sub_", "payment_method_id": "pm_"}
	for field, parse := range kinds {
		t.Run(field, func(t *testing.T) {
			for _, blank := range []string{"", " ", "\t  \n"} {
				isZero, err := parse(blank)
				require.NoError(t, err, "blank parses to the zero id, not an error")
				require.True(t, isZero)
				require.Error(t, Require(field, uuid.Nil), "and the zero id is then refused")
			}
			_, err := parse(prefixes[field] + uuid.Nil.String())
			require.Error(t, err, "an explicit zero uuid is refused at parse")
		})
	}
}
