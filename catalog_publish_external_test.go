package openrails_test

import (
	"context"

	"github.com/open-rails/openrails"
)

// An application needs only the shared Client and public declaration types.
var _ func(*openrails.CatalogClient, context.Context, *openrails.CatalogApplyParams, ...openrails.RequestOption) (*openrails.CatalogApplicationReceipt, error) = (*openrails.CatalogClient).Apply
var _ func(*openrails.CatalogClient, context.Context, ...openrails.RequestOption) (*openrails.CatalogRevision, error) = (*openrails.CatalogClient).Revision
var _ = openrails.CatalogApplyParams{SchemaVersion: 1}
