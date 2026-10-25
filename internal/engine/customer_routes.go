package engine

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/routebundle"
)

func (e *Engine) CustomerRoutes(sel config.CustomerRoutes) ([]routebundle.Route, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, fmt.Errorf("openrails: engine is closed")
	}
	if sel.Prefix == "" {
		sel.Prefix = "/v1/me"
	}
	// Literal segments retain the ordinary mount policy. Native whole-segment
	// parameters are validated with the complete route table by ServeMux below.
	parts := strings.Split(sel.Prefix, "/")
	for i, part := range parts {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			name := strings.TrimSuffix(strings.TrimPrefix(part, "{"), "}")
			if name == "" || strings.ContainsAny(name, "{}./") {
				return nil, fmt.Errorf("openrails: invalid CustomerRoutes.Prefix parameter")
			}
			parts[i] = "parameter"
		}
	}
	if err := config.ValidateMountPath("CustomerRoutes.Prefix", strings.Join(parts, "/")); err != nil {
		return nil, err
	}
	table, err := embedhttp.BuildMappedCustomerRoutes(e.App, sel)
	if err != nil {
		return nil, err
	}
	for i := range table.Entries {
		table.Entries[i].Path = sel.Prefix + strings.TrimPrefix(table.Entries[i].Path, "/v1/me")
	}
	if err := embedhttp.ValidateRouteTable(table); err != nil {
		return nil, err
	}
	return e.guardRouteLifetimes(routebundle.FromTable(table)), nil
}
