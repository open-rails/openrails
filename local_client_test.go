package openrails

import (
	"errors"
	"testing"

	"github.com/open-rails/openrails/billing"
)

// A LocalClient not made by Engine.LocalMerchant refuses every call.
func TestZeroLocalClientRefuses(t *testing.T) {
	for _, c := range []*LocalClient{{}, nil, {client: &Client{}}} {
		calls := map[string]error{}
		_, calls["StartRunningChargeTx"] = c.StartRunningChargeTx(t.Context(), nil, billing.StartRunningChargeParams{})
		_, calls["StopRunningChargeTx"] = c.StopRunningChargeTx(t.Context(), nil, "reference", billing.StopRunningChargeParams{})
		_, calls["AdmitTx"] = c.AdmitTx(t.Context(), nil, []billing.AdmitParams{{}})
		_, calls["CaptureAdmissionsTx"] = c.CaptureAdmissionsTx(t.Context(), nil, []billing.CaptureAdmissionParams{{}})
		_, calls["ReleaseAdmissionsTx"] = c.ReleaseAdmissionsTx(t.Context(), nil, []string{"request"})
		_, calls["ExportNotificationAdoption"] = c.ExportNotificationAdoption(t.Context())
		calls["RestoreNotificationOrdering"] = c.RestoreNotificationOrdering(t.Context(), billing.NotificationAdoption{})
		for name, err := range calls {
			if !errors.Is(err, errNoLocalClient) {
				t.Errorf("%s on %#v = %v, want the LocalClient refusal", name, c, err)
			}
		}
	}
}
