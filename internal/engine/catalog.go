package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/retry"
	"github.com/open-rails/openrails/internal/service"
)

// declaredCatalogProviderWait bounds New's provider reference reads; past it
// the application finishes in the background.
const declaredCatalogProviderWait = 10 * time.Second

// declaredCatalog validates Config.Catalog before anything opens and returns
// a private copy, so the host's later edits cannot reach a background retry.
func declaredCatalog(cfg config.Config) (*catalog.Application, error) {
	if cfg.Catalog == nil {
		return nil, nil
	}
	switch {
	case cfg.ControlPlane != nil:
		return nil, fmt.Errorf("openrails: Config.Catalog declares an embedded merchant's catalog; a control plane's merchants manage theirs through the API")
	case strings.TrimSpace(cfg.Merchant.Slug) == "":
		return nil, fmt.Errorf("openrails: Config.Catalog declares Config.Merchant's catalog; set Config.Merchant")
	}
	if err := cfg.Catalog.Validate(); err != nil {
		return nil, fmt.Errorf("openrails: Config.Catalog: %w", err)
	}
	raw, err := json.Marshal(cfg.Catalog)
	if err != nil {
		return nil, fmt.Errorf("openrails: Config.Catalog: %w", err)
	}
	out, err := catalog.ParseApplicationJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("openrails: Config.Catalog: %w", err)
	}
	return out, nil
}

// applyDeclaredCatalog applies Config.Catalog before New returns. A refusal
// fails New; a transient database error is retried within ctx. A provider
// reference not confirmed in time leaves the application to the background,
// and Ready fails until it commits.
func (e *Engine) applyDeclaredCatalog(ctx context.Context, params catalog.Application) error {
	rt := e.App.Runtime
	mid := rt.ConfiguredMerchant()
	for attempt := 0; ; attempt++ {
		_, err := e.svc.ApplyDeclaredCatalog(merchant.WithID(ctx, mid), params, time.Now().Add(declaredCatalogProviderWait))
		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil:
			return fmt.Errorf("openrails: apply Config.Catalog: %w", ctx.Err())
		case errors.Is(err, service.ErrCatalogProviderUnconfirmed):
			rt.ReportDeclaredCatalog(fmt.Errorf("declared catalog not applied yet: %w", err))
			e.finishDeclaredCatalog(mid, params)
			return nil
		case !transientDatabaseError(err):
			return fmt.Errorf("openrails: apply Config.Catalog: %w", err)
		}
		log.WithError(err).Warn("openrails: applying Config.Catalog; retrying")
		if !retry.Sleep(ctx, retry.Backoff(attempt, retry.Base, retry.Max)) {
			return fmt.Errorf("openrails: apply Config.Catalog: %w", err)
		}
	}
}

// finishDeclaredCatalog retries the application until it commits, with no
// provider deadline. A provider that now refuses a reference keeps Ready
// failing with that reason until the reference is fixed.
func (e *Engine) finishDeclaredCatalog(mid billing.MerchantID, params catalog.Application) {
	rt := e.App.Runtime
	rt.Go("declared catalog", func(ctx context.Context) {
		last := ""
		err := retry.Forever(ctx, func(ctx context.Context) error {
			_, err := e.svc.ApplyDeclaredCatalog(merchant.WithID(ctx, mid), params, time.Time{})
			return err
		}, func(_ int, err error) {
			rt.ReportDeclaredCatalog(fmt.Errorf("declared catalog not applied yet: %w", err))
			if err.Error() != last {
				last = err.Error()
				log.WithError(err).Warn("openrails: Config.Catalog awaits its providers; retrying in the background")
			}
		})
		if err == nil {
			rt.ReportDeclaredCatalog(nil)
			log.Info("openrails: Config.Catalog applied")
		}
	})
}

// transientDatabaseError is a database failure worth retrying: a lost or
// refused connection, a timeout, a serialization or deadlock rollback, or a
// server out of connections or restarting.
func transientDatabaseError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case strings.HasPrefix(pgErr.Code, "08"), strings.HasPrefix(pgErr.Code, "40"), strings.HasPrefix(pgErr.Code, "53"):
			return true
		}
		return pgErr.Code == "57P01" || pgErr.Code == "57P02" || pgErr.Code == "57P03"
	}
	var connectErr *pgconn.ConnectError
	var netErr net.Error
	return errors.As(err, &connectErr) || errors.As(err, &netErr) || pgconn.Timeout(err) ||
		pgconn.SafeToRetry(err) || errors.Is(err, io.ErrUnexpectedEOF)
}
