package catalog

import (
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// ServiceApplier adapts an in-process *service.Service to the Applier
// interface. The facade method set already matches the interface one-to-one, so
// the *service.Service satisfies Applier directly — this constructor exists to
// make the in-process wiring explicit and to give a single place to evolve the
// adapter if the facade signatures ever diverge.
//
// NewServiceApplier returns the service as an Applier. A compile-time assertion
// below guarantees *service.Service implements every method.
func NewServiceApplier(svc *billingservice.Service) Applier {
	return svc
}

// Compile-time guarantee that the OpenRails facade satisfies Applier.
var _ Applier = (*billingservice.Service)(nil)
