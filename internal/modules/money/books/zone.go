package books

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
)

// zones caches loaded zones by name.
var zones sync.Map

// AccountingZone is the zone whose midnights on the 1st end the merchant's
// accounting months: its accounting.timezone, UTC when unset or when no
// configuration is bound. It lives in the merchant's configuration, never in
// PostgreSQL, so a change moves only boundaries not yet posted.
func AccountingZone(ctx context.Context, d *db.DB) (*time.Location, error) {
	cfg, _, err := merchantconfig.NewStore(d).Get(ctx)
	switch {
	case errors.Is(err, merchantconfig.ErrNoSettings):
		return time.UTC, nil
	case err != nil:
		return nil, err
	case cfg.AccountingTimezone == "" || cfg.AccountingTimezone == merchantconfig.DefaultAccountingTimezone:
		return time.UTC, nil
	}
	if z, ok := zones.Load(cfg.AccountingTimezone); ok {
		return z.(*time.Location), nil
	}
	z, err := time.LoadLocation(cfg.AccountingTimezone)
	if err != nil {
		return nil, err
	}
	zones.Store(cfg.AccountingTimezone, z)
	return z, nil
}
