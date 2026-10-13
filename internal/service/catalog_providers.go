package service

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// OpenRails writes no provider catalog: our prices are billed by OpenRails,
// and provider prices are read from their provider. The only object it
// publishes for a price is the on-chain Solana plan a recurring price of ours
// sells on.

// errRemoteWritesDisabled: the operating mode (provider_write_mode limited or
// readonly) blocks publishing a Solana plan.
var errRemoteWritesDisabled = errors.New("provider writes are disabled (mode=limited/readonly)")

// ErrTrialUnsupportedOnRail refuses a trial first phase on a rail that cannot
// run one. Exported so the HTTP layer answers 400.
var ErrTrialUnsupportedOnRail = apperr.New(http.StatusBadRequest, "trial_unsupported_on_rail", "trial first phase is not supported on this rail")

// ErrPriceNotSellable refuses a price none of its methods can sell.
var ErrPriceNotSellable = apperr.New(http.StatusBadRequest, "price_not_sellable", "no PSP can sell this price")

// solanaPlanContext is the price a Solana plan is published for.
type solanaPlanContext struct {
	PriceID              uuid.UUID
	Product              *models.Product
	UnitAmount           int64
	Currency             string
	BillingCycleDays     *int
	BillingIntervalHours *int
	RemoteWritesDisabled bool
}

// normalizeLinkMap trims each key/value and drops empty pairs; nil when none
// survive.
func normalizeLinkMap(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k != "" && v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// catalogRemoteWritesDisabled reports whether the operating mode blocks
// publishing provider objects. Reads stay on.
func (s *Service) catalogRemoteWritesDisabled() bool {
	return s != nil && s.rt != nil && s.rt.Config != nil && config.IsLimitedMode(s.rt.Config)
}
