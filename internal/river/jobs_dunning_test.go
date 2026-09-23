//go:build integration

package riverjobs

import (
	"context"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// fakeDunningNMIResolver is a static money.NMIClientResolver stand-in for the
// #725 store-armed builder.
type fakeDunningNMIResolver struct{ client *nmi.NMIClient }

func (f fakeDunningNMIResolver) ResolveNMIClient(_ context.Context, _ uuid.UUID, _ *uuid.UUID) (*nmi.NMIClient, bool, error) {
	if f.client == nil {
		return nil, false, nil
	}
	return f.client, true, nil
}
