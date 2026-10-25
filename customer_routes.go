package openrails

import "fmt"

// CustomerRoutes returns the customer operations under selection.Prefix. It is
// available on embedded engines and Server.Engine. The host authorizes the
// mapping; OpenRails preserves the original verified request and reuses its
// customer handlers. No staff, provider or network-link route is included.
func (e *Engine) CustomerRoutes(selection CustomerRoutes) ([]Route, error) {
	if e == nil || e.engine == nil {
		return nil, fmt.Errorf("openrails: an initialized Engine is required")
	}
	routes, err := e.engine.CustomerRoutes(selection)
	if err != nil {
		return nil, err
	}
	out := make([]Route, len(routes))
	for i, route := range routes {
		out[i] = Route{Method: route.Method, Path: route.Path, Handler: route.Handler}
	}
	return out, nil
}
