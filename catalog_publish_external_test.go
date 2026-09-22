package openrails_test

import (
	"context"

	"github.com/open-rails/openrails"
)

// An application needs only the shared Client and public declaration types.
var _ func(*openrails.CatalogClient, context.Context, *openrails.CatalogApplyParams) (*openrails.CatalogApplicationReceipt, error) = (*openrails.CatalogClient).Apply
var _ = openrails.CatalogApplyParams{SchemaVersion: 1}
