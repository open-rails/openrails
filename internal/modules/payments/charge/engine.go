package charge

import (
	"errors"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
)

// ValidateEngineInstrument checks an accepted engine operation's execution
// binding. A card's custody selects its transport, never its schedule owner.
// Admission separately validates that the saved row and account are usable.
func ValidateEngineInstrument(rail string, instrument FrozenInstrument, binding *HyperSwitchBinding, renewal bool) error {
	if err := instrument.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(instrument.RailCustomerRef) == "" || strings.TrimSpace(instrument.RailMethodRef) == "" {
		return errors.New("engine collection requires an exact saved customer and method")
	}
	if renewal && strings.TrimSpace(instrument.StoredCredentialRecurringRef) == "" {
		return errors.New("engine renewal requires a qualified recurring agreement")
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
