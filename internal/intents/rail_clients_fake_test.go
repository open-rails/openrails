package intents

import (
	"context"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// fakeNMIResolver is a static money.NMIClientResolver: one client for every
// merchant (a test stand-in for the #725 store-armed builder).
type fakeNMIResolver struct {
	client *nmi.NMIClient
	err    error
}

func (f fakeNMIResolver) ResolveNMIClient(_ context.Context, _ uuid.UUID, _ *uuid.UUID) (*nmi.NMIClient, bool, error) {
	if f.err != nil {
		return nil, false, f.err
	}
	if f.client == nil {
		return nil, false, nil
	}
	return f.client, true, nil
}
