package merchantconfig

import (
	"fmt"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// ErrCardCustodyReserved refuses card_custody hosted, which no deployment
// serves yet.
var ErrCardCustodyReserved = apperr.New(http.StatusBadRequest, "card_custody_reserved",
	"card_custody hosted is reserved for a private vault the platform will host; choose platform or merchant").WithParam("settings.card_custody")

// CardCustody is where the merchant's new cards are stored; absent is
// merchant.
func CardCustody(settings billing.MerchantSettings) billing.CardCustody {
	if settings.CardCustody == "" {
		return billing.CardCustodyMerchant
	}
	return settings.CardCustody
}

func validateCardCustody(custody billing.CardCustody) error {
	switch custody {
	case "", billing.CardCustodyMerchant, billing.CardCustodyPlatform:
		return nil
	case billing.CardCustodyHosted:
		return ErrCardCustodyReserved
	}
	return fmt.Errorf("card_custody must be %q or %q", billing.CardCustodyPlatform, billing.CardCustodyMerchant)
}
