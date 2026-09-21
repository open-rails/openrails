package catalog

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/catalogscope"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/merchant"
)

var (
	ErrOwnerOperation = apperr.New(http.StatusForbidden, "catalog_owner_forbidden", "catalog owners cannot change merchant-wide catalog settings")
	ErrOwnerScope     = apperr.New(http.StatusForbidden, "catalog_scope_mismatch", "catalog scope does not match the authorized merchant and catalog")
)

// ValidateOwnerScope catches a merchant context replaced after owner authority
// was attached. An absent owner scope retains the administrator/buyer behavior.
func ValidateOwnerScope(ctx context.Context) error {
	if ctx == nil {
		return merchant.ErrNoMerchant
	}
	owner, ok := catalogscope.FromContext(ctx)
	if !ok {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	if owner.MerchantID != mid || owner.CatalogID == uuid.Nil || catalogscope.ValidateSubject(owner.OwnerSubject) != nil {
		return ErrOwnerScope
	}
	return nil
}

// RefuseOwnerOperation is for merchant-wide maintenance and provider controls.
func RefuseOwnerOperation(ctx context.Context) error {
	if err := ValidateOwnerScope(ctx); err != nil {
		return err
	}
	if _, owned := catalogscope.FromContext(ctx); owned {
		return ErrOwnerOperation
	}
	return nil
}

func queryCatalogScope(ctx context.Context) (merchant.ID, *uuid.UUID, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return merchant.ID{}, nil, err
	}
	if err := ValidateOwnerScope(ctx); err != nil {
		return merchant.ID{}, nil, err
	}
	return mid, catalogscope.QueryID(ctx), nil
}

func queryCatalogMerchant(ctx context.Context, requested uuid.UUID) (*uuid.UUID, error) {
	mid, catalogID, err := queryCatalogScope(ctx)
	if err != nil {
		return nil, err
	}
	if requested != mid.UUID() {
		return nil, ErrOwnerScope
	}
	return catalogID, nil
}

func selectCatalogFilter(ctx context.Context, database *db.DB, requested *uuid.UUID) (*uuid.UUID, error) {
	_, owned, err := queryCatalogScope(ctx)
	if err != nil {
		return nil, err
	}
	if owned != nil {
		if requested != nil && *requested != *owned {
			return nil, ErrOwnerScope
		}
		return owned, nil
	}
	if requested != nil {
		if *requested == uuid.Nil {
			return nil, apperr.Invalidf("catalog_id must not be zero")
		}
		if _, err := NewCatalogRepo(database).Get(ctx, *requested); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, apperr.New(http.StatusNotFound, "catalog_not_found", "catalog not found")
			}
			return nil, err
		}
	}
	return requested, nil
}
