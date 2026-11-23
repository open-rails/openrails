package guide

import (
	"context"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// RentMachine bills a customer by the hour while a machine runs.
func RentMachine(ctx context.Context, shop *openrails.Client, customer billing.CustomerID, machine string) (func(context.Context) error, error) {
	// guide:running-charge
	_, err := shop.StartRunningCharge(ctx, billing.StartRunningChargeParams{
		Reference:     "machine-" + machine, // the identity: a retry with the same terms answers the same charge
		CustomerID:    customer,
		Currency:      "USD",
		RatePerHour:   1_200_000, // $1.20 an hour, accrued per second
		MinimumAmount: 1_200_000, // refused (402) unless an hour is covered
	})
	if err != nil {
		return nil, err
	}
	stop := func(ctx context.Context) error {
		_, err := shop.StopRunningCharge(ctx, "machine-"+machine, billing.StopRunningChargeParams{})
		return err
	}
	// guide:end
	return stop, nil
}
