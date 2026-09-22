//go:build integration

// Package httptesthost assembles the production route inventory around an existing
// fixture graph. It lets business/security integration cases vary identity policy
// without creating a second engine or reaching private Runtime fields.
package httptesthost

import (
	"net/http"
	"strings"

	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/pkg/billingauth"
)

type Options struct {
	HTTP                   embed.HTTPConfig
	DelegatedAuthenticator billingauth.DelegatedAuthenticator
	Prefix                 string
}

func Handler(runtime *embed.Runtime, opts Options) (http.Handler, error) {
	table, err := embedhttp.ConfiguredRoutes(app.HostGraph(runtime), &opts.HTTP, opts.DelegatedAuthenticator)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	for _, route := range table.Entries {
		mux.Handle(route.Method+" "+strings.TrimRight(opts.Prefix, "/")+strings.TrimPrefix(route.Path, "/billing"), route.Handler)
	}
	return mux, nil
}
