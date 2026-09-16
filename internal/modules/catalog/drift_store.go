package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// DriftResource identifies one local catalog object.
type DriftResource struct {
	Type models.CatalogDriftResourceType
	ID   string
}

// DriftCoverage is what a successful provider read proves absent. A nil
// Resource means the PSP account's whole catalog was enumerated.
type DriftCoverage struct {
	PSPID    uuid.UUID
	Resource *DriftResource
}

type driftKey struct {
	pspID                                          uuid.UUID
	kind, resourceType, localID, externalID, field string
}

// PersistDrift upserts observed findings and resolves open ones the coverage
// proves absent. Disabled, failed or partial reads resolve nothing, and an older
// snapshot never overwrites or resolves newer evidence. newEvents counts
// findings that became open; ignored identities stay ignored and do not count.
func PersistDrift(ctx context.Context, database *db.DB, desired []models.CatalogDriftEvent, coverage []DriftCoverage, now time.Time) (newEvents, resolved int, err error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range desired {
		if e.PSPID == uuid.Nil {
			return 0, 0, fmt.Errorf("catalog drift %s/%s has no PSP identity", e.Provider, e.Kind)
		}
	}
	err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		rows, err := q.ListOpenCatalogDriftEvents(ctx)
		if err != nil {
			return err
		}
		open := make(map[driftKey]gen.OpenrailsCatalogDriftEvent, len(rows))
		for _, r := range rows {
			open[driftKey{driftPSP(r.PspID), r.Kind, r.OpenrailsResourceType, driftText(r.OpenrailsResourceID), driftText(r.ExternalResourceID), driftText(r.Field)}] = r
		}
		seen := make(map[driftKey]bool, len(desired))
		for _, e := range desired {
			k := driftKey{e.PSPID, string(e.Kind), string(e.OpenRailsResourceType), e.OpenRailsResourceID, e.ExternalResourceID, e.Field}
			if seen[k] {
				continue
			}
			seen[k] = true
			id := e.ID
			if id == uuid.Nil {
				id = uuidutil.NewV7()
			}
			status, err := q.UpsertCatalogDriftFinding(ctx, gen.UpsertCatalogDriftFindingParams{
				ID: id, MerchantID: mid.UUID(), Kind: k.kind, PspID: k.pspID, Rail: string(e.Provider),
				OpenrailsResourceType: k.resourceType, OpenrailsResourceID: driftOptional(k.localID),
				ExternalResourceID: driftOptional(k.externalID), Field: driftOptional(k.field),
				OpenrailsValue: driftOptional(e.OpenRailsValue), ExternalValue: driftOptional(e.ExternalValue), ObservedAt: now,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				continue // a newer observation already owns this finding
			}
			if err != nil {
				return err
			}
			if _, wasOpen := open[k]; !wasOpen && status != "ignored" {
				newEvents++
			}
		}
		for k, row := range open {
			if seen[k] || !covers(coverage, row) {
				continue
			}
			n, err := q.ResolveCatalogDriftFinding(ctx, gen.ResolveCatalogDriftFindingParams{ID: row.ID, PspID: driftPSP(row.PspID), ResolvedAt: now})
			if err != nil {
				return err
			}
			resolved += int(n)
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return newEvents, resolved, nil
}

func covers(coverage []DriftCoverage, row gen.OpenrailsCatalogDriftEvent) bool {
	for _, c := range coverage {
		if c.PSPID != driftPSP(row.PspID) {
			continue
		}
		if c.Resource == nil || (string(c.Resource.Type) == row.OpenrailsResourceType && c.Resource.ID == driftText(row.OpenrailsResourceID)) {
			return true
		}
	}
	return false
}

func driftPSP(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

func driftText(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func driftOptional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
