package openrails_test

import (
	"context"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/pkg/catalog"
)

// An application needs only the shared Client and public declaration types.
var _ func(*openrails.Client, context.Context, openrails.CatalogPublishRequest, ...openrails.RequestOption) (*openrails.CatalogPublishResponse, error) = (*openrails.Client).PublishCatalog
var _ = openrails.CatalogPublishRequest{Catalog: catalog.Manifest{Version: catalog.SupportedVersion}}
