package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/open-rails/openrails/internal/railresolve"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

// fakeStripeFeatures is a tiny stateful stand-in for Stripe's Entitlements +
// Product-Features API, enough to exercise SyncProductFeatures end to end.
type fakeStripeFeatures struct {
	mu        sync.Mutex
	featByKey map[string]feat              // lookup_key -> feature
	attached  map[string]map[string]string // productID -> (productFeatureID -> lookup_key)
	nFeat     int
	nPF       int

	createFeat int
	attach     int
	detach     int
}

type feat struct {
	id        string
	lookupKey string
	name      string
	managed   bool
}

func newFakeStripeFeatures() *fakeStripeFeatures {
	return &fakeStripeFeatures{
		featByKey: map[string]feat{},
		attached:  map[string]map[string]string{},
	}
}

func (f *fakeStripeFeatures) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = r.ParseForm()
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // v1 / ...
		w.Header().Set("Content-Type", "application/json")

		switch {
		// /v1/entitlements/features
		case len(parts) == 3 && parts[1] == "entitlements" && parts[2] == "features":
			if r.Method == http.MethodPost {
				f.createFeat++
				key := r.Form.Get("lookup_key")
				ft := feat{
					id:        fmt.Sprintf("feat_%d", f.nFeat),
					lookupKey: key,
					name:      r.Form.Get("name"),
					managed:   r.Form.Get("metadata[openrails_managed]") == "true",
				}
				f.nFeat++
				f.featByKey[key] = ft
				writeFeatJSON(w, ft)
				return
			}
			// GET list
			var data []string
			for _, ft := range f.featByKey {
				data = append(data, featJSON(ft))
			}
			sort.Strings(data)
			fmt.Fprintf(w, `{"object":"list","data":[%s],"has_more":false}`, strings.Join(data, ","))
			return

		// /v1/products/{pid}/features  and  /v1/products/{pid}/features/{pfid}
		case len(parts) >= 4 && parts[1] == "products" && parts[3] == "features":
			pid := parts[2]
			if len(parts) == 4 && r.Method == http.MethodPost {
				f.attach++
				featID := r.Form.Get("entitlement_feature")
				lk := ""
				for _, ft := range f.featByKey {
					if ft.id == featID {
						lk = ft.lookupKey
					}
				}
				pfID := fmt.Sprintf("pf_%d", f.nPF)
				f.nPF++
				if f.attached[pid] == nil {
					f.attached[pid] = map[string]string{}
				}
				f.attached[pid][pfID] = lk
				fmt.Fprintf(w, `{"id":%q,"object":"product_feature"}`, pfID)
				return
			}
			if len(parts) == 4 { // GET list
				var data []string
				for pfID, lk := range f.attached[pid] {
					ft := f.featByKey[lk]
					data = append(data, fmt.Sprintf(`{"id":%q,"entitlement_feature":%s}`, pfID, featJSON(ft)))
				}
				sort.Strings(data)
				fmt.Fprintf(w, `{"object":"list","data":[%s],"has_more":false}`, strings.Join(data, ","))
				return
			}
			if len(parts) == 5 && r.Method == http.MethodDelete { // detach
				f.detach++
				pfID := parts[4]
				delete(f.attached[pid], pfID)
				fmt.Fprintf(w, `{"id":%q,"object":"product_feature","deleted":true}`, pfID)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"message":"unhandled"}}`)
	}
}

func featJSON(ft feat) string {
	managed := "false"
	if ft.managed {
		managed = "true"
	}
	return fmt.Sprintf(`{"id":%q,"lookup_key":%q,"name":%q,"metadata":{"openrails_managed":%q}}`,
		ft.id, ft.lookupKey, ft.name, managed)
}

func writeFeatJSON(w http.ResponseWriter, ft feat) {
	var out map[string]any
	_ = json.Unmarshal([]byte(featJSON(ft)), &out)
	_ = json.NewEncoder(w).Encode(out)
}

// attachedKeys returns the sorted lookup_keys attached to a product.
func (f *fakeStripeFeatures) attachedKeys(pid string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, lk := range f.attached[pid] {
		out = append(out, lk)
	}
	sort.Strings(out)
	return out
}

func newFeatureTestSvc(t *testing.T, fake *fakeStripeFeatures) *StripeCatalogService {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return &StripeCatalogService{
		Rails:   railresolve.FixedSet{"stripe": {Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_123"}}},
		BaseURL: srv.URL,
	}
}

func TestSyncProductFeatures(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeFeatures()
	svc := newFeatureTestSvc(t, fake)
	const pid = "prod_1"

	// 1. From nothing: create + attach both desired features.
	require.NoError(t, svc.SyncProductFeatures(ctx, pid, []string{"premium", "pro"}))
	require.Equal(t, []string{"premium", "pro"}, fake.attachedKeys(pid))
	require.Equal(t, 2, fake.createFeat)
	require.Equal(t, 2, fake.attach)
	require.Equal(t, 0, fake.detach)

	// 2. Idempotent rerun: no new creates/attaches/detaches.
	require.NoError(t, svc.SyncProductFeatures(ctx, pid, []string{"pro", "premium"}))
	require.Equal(t, 2, fake.createFeat)
	require.Equal(t, 2, fake.attach)
	require.Equal(t, 0, fake.detach)

	// 3. Drop "pro": it is detached, "premium" stays. The feature is reused
	//    (not recreated) so createFeat is unchanged.
	require.NoError(t, svc.SyncProductFeatures(ctx, pid, []string{"premium"}))
	require.Equal(t, []string{"premium"}, fake.attachedKeys(pid))
	require.Equal(t, 1, fake.detach)
	require.Equal(t, 2, fake.createFeat)

	// 4. Re-add "pro": feature already exists, so only an attach (no create).
	require.NoError(t, svc.SyncProductFeatures(ctx, pid, []string{"premium", "pro"}))
	require.Equal(t, []string{"premium", "pro"}, fake.attachedKeys(pid))
	require.Equal(t, 2, fake.createFeat)
	require.Equal(t, 3, fake.attach)

	// 5. Empty desired set: detach all OpenRails-managed features.
	require.NoError(t, svc.SyncProductFeatures(ctx, pid, nil))
	require.Empty(t, fake.attachedKeys(pid))
}

// TestSyncProductFeaturesLeavesUnmanagedAlone proves an operator-attached feature
// (no openrails_managed marker) is never detached by a sync.
func TestSyncProductFeaturesLeavesUnmanagedAlone(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeFeatures()
	svc := newFeatureTestSvc(t, fake)
	const pid = "prod_1"

	// Seed an operator-created (unmanaged) feature already attached to the product.
	fake.featByKey["legacy"] = feat{id: "feat_legacy", lookupKey: "legacy", name: "legacy", managed: false}
	fake.attached[pid] = map[string]string{"pf_legacy": "legacy"}

	// Sync a disjoint desired set; "legacy" must survive (it's not ours to remove).
	require.NoError(t, svc.SyncProductFeatures(ctx, pid, []string{"premium"}))
	require.Equal(t, []string{"legacy", "premium"}, fake.attachedKeys(pid))
	require.Equal(t, 0, fake.detach)
}
