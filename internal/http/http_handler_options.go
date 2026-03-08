package server

// HTTPHandlerOptions controls which billing HTTP route groups are registered on the returned handler.
//
// If all fields are false (zero value), the options default to the full public surface:
// user + admin + webhooks.
//
// Note: health/debug endpoints are intentionally not part of embedded handler options.
// Standalone mode owns service-level health routes.
type HTTPHandlerOptions struct {
	IncludeUser     bool
	IncludeAdmin    bool
	IncludeWebhooks bool
}

func (o HTTPHandlerOptions) isZero() bool {
	return !o.IncludeUser && !o.IncludeAdmin && !o.IncludeWebhooks
}

func (o HTTPHandlerOptions) withDefaults() HTTPHandlerOptions {
	if !o.isZero() {
		return o
	}
	return HTTPHandlerOptions{
		IncludeUser:     true,
		IncludeAdmin:    true,
		IncludeWebhooks: true,
	}
}
