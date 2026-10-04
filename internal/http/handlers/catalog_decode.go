package handlers

import (
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// bindCatalogJSON decodes a catalog declaration with the one strict decoder:
// an unknown field is refused, so a retired benefit is never silently ignored.
func bindCatalogJSON(r *httprequest.Request, out any) bool { return r.BindJSON(out) }
