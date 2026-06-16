package entitlements

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These unit tests cover the FeatureService input-validation paths, which all
// run BEFORE any database access — so a service over a nil DB exercises them
// without a live Postgres. The DB-backed behaviour (merchant isolation, real
// inserts/reads) is covered by the //go:build integration repo test.

func newValidationService() *FeatureService {
	// repo/ents wrap a nil *db.DB; validation returns before they are dereferenced.
	return NewFeatureService(nil)
}

func TestCreateFeature_RequiresLookupKey(t *testing.T) {
	svc := newValidationService()
	_, err := svc.CreateFeature(context.Background(), CreateFeatureParams{Name: "Premium"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "lookup_key")
}

func TestCreateFeature_RequiresName(t *testing.T) {
	svc := newValidationService()
	_, err := svc.CreateFeature(context.Background(), CreateFeatureParams{LookupKey: "premium"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "name")
}

func TestCreateFeature_TrimsAndRejectsBlank(t *testing.T) {
	svc := newValidationService()
	_, err := svc.CreateFeature(context.Background(), CreateFeatureParams{LookupKey: "   ", Name: "Premium"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "lookup_key")
}

func TestAttachFeature_RequiresProductID(t *testing.T) {
	svc := newValidationService()
	_, err := svc.AttachFeatureToProduct(context.Background(), AttachFeatureParams{
		EntitlementFeatureID: uuid.New(),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "product_id")
}

func TestAttachFeature_RequiresFeatureID(t *testing.T) {
	svc := newValidationService()
	_, err := svc.AttachFeatureToProduct(context.Background(), AttachFeatureParams{
		ProductID: uuid.New(),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "entitlement_feature_id")
}

func TestAttachFeature_RejectsNonPositiveDuration(t *testing.T) {
	svc := newValidationService()
	zero := 0
	_, err := svc.AttachFeatureToProduct(context.Background(), AttachFeatureParams{
		ProductID:            uuid.New(),
		EntitlementFeatureID: uuid.New(),
		DurationDays:         &zero,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "duration_days")
}

func TestGetActiveEntitlements_RequiresUserID(t *testing.T) {
	svc := newValidationService()
	_, err := svc.GetActiveEntitlements(context.Background(), "   ", time.Time{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "userID")
}
