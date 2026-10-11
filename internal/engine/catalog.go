package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
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

// declaredCatalog validates a declared merchant's catalog before anything
// opens and returns a private copy, so the host's later edits cannot reach a
// background retry.
func declaredCatalog(m config.MerchantDeclaration) (*catalog.Application, error) {
	if m.Catalog == nil {
		return nil, nil
	}
	name := fmt.Sprintf("openrails: Config.Merchants[%q].Catalog", m.Slug)
	if err := m.Catalog.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	raw, err := json.Marshal(m.Catalog)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	out, err := catalog.ParseApplicationJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// reportCatalog records why slug's declared catalog is not applied yet (nil
// once it is): Ready fails while any is not.
func (e *Engine) reportCatalog(slug string, err error) {
	e.catalogMu.Lock()
	defer e.catalogMu.Unlock()
	if e.catalogPending == nil {
		e.catalogPending = map[string]error{}
	}
	if err == nil {
		delete(e.catalogPending, slug)
	} else {
		e.catalogPending[slug] = fmt.Errorf("merchant %s: declared catalog not applied yet: %w", slug, err)
	}
	var pending []error
	for _, slug := range slices.Sorted(maps.Keys(e.catalogPending)) {
		pending = append(pending, e.catalogPending[slug])
	}
	e.App.Runtime.ReportDeclaredCatalog(errors.Join(pending...))
}

// applyDeclaredCatalog applies the catalog merchant slug declares before New
// returns. A refusal fails New; a transient database error is retried within
// ctx. An object an edit changed is skipped, not refused: New succeeds, the
// finding names it and the next start retries it. A provider reference not
// confirmed in time leaves the application to the background, and Ready fails
// until it commits.
func (e *Engine) applyDeclaredCatalog(ctx context.Context, mid billing.MerchantID, slug string, params catalog.Application) error {
	name := fmt.Sprintf("openrails: apply Config.Merchants[%q].Catalog", slug)
	for attempt := 0; ; attempt++ {
		receipt, err := e.svc.ApplyDeclaredCatalog(merchant.WithID(ctx, mid), params, time.Now().Add(declaredCatalogProviderWait))
		switch {
		case err == nil:
			logCatalogConflicts(slug, receipt)
			return nil
		case ctx.Err() != nil:
			return fmt.Errorf("%s: %w", name, ctx.Err())
		case errors.Is(err, service.ErrCatalogProviderUnconfirmed):
			e.reportCatalog(slug, err)
			e.finishDeclaredCatalog(mid, slug, params)
			return nil
		case !transientDatabaseError(err):
			return fmt.Errorf("%s: %w", name, err)
		}
		log.WithError(err).WithField("merchant", slug).Warn("openrails: applying the declared catalog; retrying")
		if !retry.Sleep(ctx, retry.Backoff(attempt, retry.Base, retry.Max)) {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
}

// finishDeclaredCatalog retries the application until it commits, with no
// provider deadline. A provider that now refuses a reference keeps Ready
// failing with that reason until the reference is fixed.
func (e *Engine) finishDeclaredCatalog(mid billing.MerchantID, slug string, params catalog.Application) {
	rt := e.App.Runtime
	rt.Go("declared catalog", func(ctx context.Context) {
		last := ""
		err := retry.Forever(ctx, func(ctx context.Context) error {
			receipt, err := e.svc.ApplyDeclaredCatalog(merchant.WithID(ctx, mid), params, time.Time{})
			if err == nil {
				logCatalogConflicts(slug, receipt)
			}
			return err
		}, func(_ int, err error) {
			e.reportCatalog(slug, err)
			if err.Error() != last {
				last = err.Error()
				log.WithError(err).WithField("merchant", slug).Warn("openrails: the declared catalog awaits its providers; retrying in the background")
			}
		})
		if err == nil {
			e.reportCatalog(slug, nil)
			log.WithField("merchant", slug).Info("openrails: declared catalog applied")
		}
	})
}

// logCatalogConflicts warns of each object the declared catalog skipped
// because an edit set its fields; the catalog finding names them too.
func logCatalogConflicts(slug string, receipt *billing.CatalogApplicationReceipt) {
	if receipt == nil {
		return
	}
	for _, line := range service.ReadableCatalogConflicts(receipt.Conflicts) {
		log.WithField("merchant", slug).Warn("openrails: declared catalog: " + line)
	}
}

// transientDatabaseError is a database failure worth retrying: a lost or
// refused connection, a timeout, a serialization or deadlock rollback, a row
// another replica holds, or a server out of connections or restarting.
func transientDatabaseError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case strings.HasPrefix(pgErr.Code, "08"), strings.HasPrefix(pgErr.Code, "40"), strings.HasPrefix(pgErr.Code, "53"):
			return true
		}
		return pgErr.Code == "55P03" || pgErr.Code == "57P01" || pgErr.Code == "57P02" || pgErr.Code == "57P03"
	}
	var connectErr *pgconn.ConnectError
	var netErr net.Error
	return errors.As(err, &connectErr) || errors.As(err, &netErr) || pgconn.Timeout(err) ||
		pgconn.SafeToRetry(err) || errors.Is(err, io.ErrUnexpectedEOF)
}
