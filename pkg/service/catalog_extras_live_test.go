//go:build stripelive

// Live (read-only) Stripe extras-listing smoke (or#896): tagged so it is
// evidence when a lane runs it and absent otherwise.
//
//	go test -tags=stripelive ./pkg/service/ -run TestLiveStripeExtrasListing -v

package service

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/railresolve"
)

// TestLiveStripeExtrasListing runs the extras-detection LISTING (GETs only;
// nothing is ever archived) against a real Stripe account. Opt-in via
// OPENRAILS_LIVE_STRIPE_KEY; use a TEST-mode key.
func TestLiveStripeExtrasListing(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("OPENRAILS_LIVE_STRIPE_KEY"))
	if key == "" {
		t.Skip("set OPENRAILS_LIVE_STRIPE_KEY (a Stripe TEST key) to run the live read-only extras listing")
	}
	rails := railresolve.FixedSet{
		"stripe": {Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: key}},
	}
	lister := &catalog.StripeCatalogService{Config: &config.Config{}, Rails: rails}
	products, prices, err := fetchStripeCatalog(context.Background(), lister)
	if err != nil {
		t.Fatalf("live stripe listing: %v", err)
	}
	// Empty local snapshot: every remote object shows up classified ours/foreign.
	extras := computeStripeExtras(products, prices, buildSnapshotFromRows(nil, nil))
	t.Logf("live stripe account: %d products, %d prices, %d extras vs an empty catalog", len(products), len(prices), len(extras))
	for _, e := range extras {
		marker := "foreign"
		if e.Owned {
			marker = "openrails-marked"
		}
		t.Logf("  %s %s (%s) active=%v [%s]", e.ObjectType, e.ExternalID, e.Label, e.Active, marker)
	}
}
