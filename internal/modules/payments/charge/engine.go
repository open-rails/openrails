package charge

import (
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
)

// ValidateEngineInstrument checks an accepted engine operation's execution
// binding. A card's custody selects its transport, never its schedule owner.
// Admission separately validates that the saved row and account are usable.
func ValidateEngineInstrument(rail string, instrument FrozenInstrument, binding *HyperSwitchBinding) error {
	if err := instrument.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(instrument.RailCustomerRef) == "" || strings.TrimSpace(instrument.RailMethodRef) == "" {
		return errors.New("engine collection requires an exact saved customer and method")
	}
	switch instrument.Custodian {
	case models.CustodianPSP:
		if binding != nil || (rail != string(models.RailNMI) && rail != string(models.RailStripe)) {
			return errors.New("engine collection has an unsupported provider instrument")
		}
	case models.CustodianHyperSwitch:
		if rail != string(models.RailNMI) || binding == nil {
			return errors.New("engine collection requires its accepted HyperSwitch binding")
		}
		return binding.Validate()
	default:
		return errors.New("custodian has no qualified engine collection transport")
	}
	return nil
}

// ErrNoRecurringMandate: a renewal needs its subscription's active recurring
// mandate with references.
var ErrNoRecurringMandate = fmt.Errorf("engine renewal requires an active recurring mandate: %w", ErrAgreementRequired)

// ValidateRenewalMandate checks a frozen renewal cites its recurring mandate.
func ValidateRenewalMandate(instrument FrozenInstrument) error {
	if m := instrument.Mandate; m == nil || m.Kind != AgreementRecurring || strings.TrimSpace(m.InitialTransactionID) == "" {
		return ErrNoRecurringMandate
	}
	return nil
}
