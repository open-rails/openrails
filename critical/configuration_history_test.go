//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

// historyOf is every entry of the merchant's configuration history the
// params select, newest first, read page by page.
func historyOf(t *testing.T, client *openrails.Client, params billing.LogListParams) []billing.LogEntry {
	t.Helper()
	var out []billing.LogEntry
	for {
		page, err := client.ListChanges(t.Context(), params)
		require.NoError(t, err)
		out = append(out, page.Items...)
		if page.Next == "" {
			return out
		}
		params.Cursor = page.Next
	}
}

// objectHistory is one object's entries, newest first.
func objectHistory(t *testing.T, client *openrails.Client, objectType billing.LogObjectType, id string) []billing.LogEntry {
	t.Helper()
	return historyOf(t, client, billing.LogListParams{ObjectType: objectType, ObjectID: id})
}

type loggedChange struct {
	Version       int                        `json:"version"`
	Revision      *int64                     `json:"revision"`
	ApplicationID string                     `json:"application_id"`
	Fields        map[string]json.RawMessage `json:"fields"`
	Keys          []string                   `json:"keys"`
	From          string                     `json:"from"`
	To            string                     `json:"to"`
}

func changeOf(t *testing.T, e billing.LogEntry) loggedChange {
	t.Helper()
	var c loggedChange
	require.NoError(t, json.Unmarshal(e.Changes, &c))
	require.Equal(t, 1, c.Version)
	return c
}

// fieldSides is one logged field's value before and after.
func fieldSides(t *testing.T, c loggedChange, field string) (before, after any) {
	t.Helper()
	raw, ok := c.Fields[field]
	require.True(t, ok, "field %s is logged: %v", field, c.Fields)
	var sides map[string]any
	require.NoError(t, json.Unmarshal(raw, &sides))
	return sides["before"], sides["after"]
}

func actions(entries []billing.LogEntry) []billing.LogAction {
	out := make([]billing.LogAction, len(entries))
	for i, e := range entries {
		out[i] = e.Action
	}
	return out
}

func TestConfigurationHistory(t *testing.T) {
	t.Run("OR-J43-S01/an-edit-and-its-entry-commit-and-roll-back-together", func(t *testing.T) {
		a := newAuthorityWorld(t)
		solo, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "solo", DisplayName: "Solo", Entitlements: []string{"hd"}})
		require.NoError(t, err)
		price, err := a.alphaRemote.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: solo.ID, Key: "monthly", UnitAmount: 5_000_000, Currency: "USD", BillingIntervalMonths: ptr(1)})
		require.NoError(t, err)
		duo, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "duo", DisplayName: "Duo", Entitlements: []string{"hd"}})
		require.NoError(t, err)

		committed := objectHistory(t, a.alphaRemote, billing.LogObjectProduct, "solo")
		require.Equal(t, []billing.LogAction{billing.LogActionKeyAdd, billing.LogActionCreate}, actions(committed))
		require.Equal(t, []string{"hd"}, changeOf(t, committed[0]).Keys)
		_, after := fieldSides(t, changeOf(t, committed[1]), "display_name")
		require.Equal(t, "Solo", after)
		for _, e := range committed {
			require.True(t, strings.HasPrefix(string(e.Actor), "staff:"), e.Actor)
			require.Equal(t, billing.LogSourceAPI, e.Source)
			require.NotNil(t, e.RequestID)
		}
		priced := objectHistory(t, a.alphaRemote, billing.LogObjectPrice, "solo:monthly")
		require.Len(t, priced, 1)
		require.Equal(t, billing.LogActionCreate, priced[0].Action)
		_, amount := fieldSides(t, changeOf(t, priced[0]), "unit_amount")
		require.Equal(t, "5000000", amount)
		require.EqualValues(t, price.Revision, *changeOf(t, priced[0]).Revision)

		// A second recurring product granting hd outside a tier group is
		// refused after the price row and its entry were written: both roll
		// back.
		everything := historyOf(t, a.alphaRemote, billing.LogListParams{})
		_, err = a.alphaRemote.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: duo.ID, Key: "monthly", UnitAmount: 5_000_000, Currency: "USD", BillingIntervalMonths: ptr(1)})
		requireCode(t, err, billing.CodeCatalogBenefitOverlap)
		require.Equal(t, everything, historyOf(t, a.alphaRemote, billing.LogListParams{}), "a refused edit records nothing")
		read, err := a.alphaRemote.GetProduct(t.Context(), duo.ID)
		require.NoError(t, err)
		require.Empty(t, read.Prices)
		require.Equal(t, duo.Revision, read.Revision)
		require.Empty(t, objectHistory(t, a.alphaRemote, billing.LogObjectPrice, "duo:monthly"))
	})

	t.Run("OR-J43-S02/a-document-apply-records-each-changed-object-once", func(t *testing.T) {
		a := newAuthorityWorld(t)
		document := func(plus, basicKeys string) *catalog.Application {
			app, err := catalog.ParseApplicationYAML([]byte(`schema_version: 1
meters:
  tokens: {event_type: chat, value_property: tokens, aggregation: sum}
limits:
  daily:
    unit: usage_units
    reset: {kind: fixed, window_seconds: 86400, anchor: first_use}
    overrun: admit_while_positive
products:
  basic:
    display_name: Basic
    entitlements: [` + basicKeys + `]
    limits: {daily: "100"}
    prices:
      monthly: {amount: 5 USD, billing_interval: 1 month}
  plus: ` + plus + `
`))
			require.NoError(t, err)
			return app
		}
		first, err := a.alphaRemote.ApplyCatalog(t.Context(), document(`{display_name: Plus, entitlements: [hd]}`, "sd"), billing.ApplyCatalogParams{})
		require.NoError(t, err)
		entries := historyOf(t, a.alphaRemote, billing.LogListParams{})
		seen := map[string]int{}
		for _, e := range entries {
			require.Equal(t, billing.LogSourceCatalogApply, e.Source)
			require.True(t, strings.HasPrefix(string(e.Actor), "staff:"), e.Actor)
			c := changeOf(t, e)
			require.NotNil(t, c.Revision, "every entry carries its object's revision")
			if e.Action != billing.LogActionKeyAdd {
				require.Equal(t, first.ApplicationID, c.ApplicationID)
			}
			seen[string(e.ObjectType)+" "+e.ObjectID+" "+string(e.Action)]++
		}
		require.Equal(t, map[string]int{
			"product basic create": 1, "product basic key_add": 1, "product plus create": 1, "product plus key_add": 1,
			"price basic:monthly create": 1, "meter tokens create": 1, "limit daily create": 1,
		}, seen)

		replay, err := a.alphaRemote.ApplyCatalog(t.Context(), document(`{display_name: Plus, entitlements: [hd]}`, "sd"), billing.ApplyCatalogParams{})
		require.NoError(t, err)
		require.True(t, replay.Replayed)
		require.Len(t, historyOf(t, a.alphaRemote, billing.LogListParams{}), len(entries), "a replay records nothing")

		_, err = a.alphaRemote.ApplyCatalog(t.Context(), document(`{display_name: Plus HD, entitlements: [hd, uhd]}`, ""), billing.ApplyCatalogParams{})
		require.NoError(t, err)
		latest := historyOf(t, a.alphaRemote, billing.LogListParams{})
		require.Len(t, latest, len(entries)+3, "only the changed objects are recorded")
		plus := objectHistory(t, a.alphaRemote, billing.LogObjectProduct, "plus")
		require.Equal(t, []billing.LogAction{billing.LogActionKeyAdd, billing.LogActionUpdate, billing.LogActionKeyAdd, billing.LogActionCreate}, actions(plus))
		require.Equal(t, []string{"uhd"}, changeOf(t, plus[0]).Keys)
		before, after := fieldSides(t, changeOf(t, plus[1]), "display_name")
		require.Equal(t, "Plus", before)
		require.Equal(t, "Plus HD", after)
		basic := objectHistory(t, a.alphaRemote, billing.LogObjectProduct, "basic")
		require.Equal(t, billing.LogActionKeyRemove, basic[0].Action, "a removed key is recorded")
		require.Equal(t, []string{"sd"}, changeOf(t, basic[0]).Keys)
		products, err := a.alphaRemote.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{"basic"}})
		require.NoError(t, err)
		require.Len(t, products.Items, 1)
		require.EqualValues(t, products.Items[0].Revision, *changeOf(t, basic[0]).Revision)
	})

	t.Run("OR-J43-S03/a-configuration-change-through-the-api-is-recorded-with-its-actor-and-no-secret", func(t *testing.T) {
		a, _ := newPSPWorld(t)
		_, err := a.alphaRemote.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{DisplayName: ptr("Alpha Shop")})
		require.NoError(t, err)
		created, err := a.alphaRemote.CreatePSP(t.Context(), pspInput())
		require.NoError(t, err)
		_, err = a.alphaRemote.UpdatePSP(t.Context(), created.ID, billing.UpdatePSPParams{Credentials: map[string]string{"secret_key": "sk_test_rotated"}})
		require.NoError(t, err)

		merchantDoc := objectHistory(t, a.alphaRemote, billing.LogObjectMerchant, a.alpha.MerchantID().String())
		require.NotEmpty(t, merchantDoc)
		named := merchantDoc[0]
		require.True(t, strings.HasPrefix(string(named.Actor), "staff:"), named.Actor)
		require.Equal(t, billing.LogSourceAPI, named.Source)
		_, after := fieldSides(t, changeOf(t, named), "display_name")
		require.Equal(t, "Alpha Shop", after)

		psp := objectHistory(t, a.alphaRemote, billing.LogObjectPSP, "stripe")
		var create, rotate *billing.LogEntry
		for i := range psp {
			e := &psp[i]
			if e.Actor != named.Actor {
				continue
			}
			switch {
			case e.Action == billing.LogActionCreate:
				create = e
			case e.Action == billing.LogActionUpdate && rotate == nil:
				rotate = e
			}
		}
		require.NotNil(t, create, "the PSP's creation is recorded as its actor's: %v", psp)
		require.NotNil(t, rotate, "the rotation is recorded as its actor's: %v", psp)
		made := changeOf(t, *create)
		for _, secret := range []string{"secrets.secret_key", "secrets.webhook_signing_secret"} {
			require.JSONEq(t, `"changed"`, string(made.Fields[secret]))
		}
		_, names := fieldSides(t, made, "secrets")
		require.ElementsMatch(t, []any{"secret_key", "webhook_signing_secret"}, names)
		_, account := fieldSides(t, made, "account_id")
		require.Equal(t, "acct_e2e", account)
		rotated := changeOf(t, *rotate)
		require.Equal(t, map[string]json.RawMessage{"secrets.secret_key": json.RawMessage(`"changed"`)}, rotated.Fields)
		require.Greater(t, *rotated.Revision, *made.Revision)

		status, raw := a.request(t, http.MethodGet, "/v1/merchant/changes", a.alphaKey.Secret, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Contains(t, string(raw), `"secrets.secret_key":"changed"`)
		assertNoPSPSecrets(t, json.RawMessage(raw))
	})

	t.Run("OR-J43-S04/a-change-made-outside-openrails-is-recorded-once-on-reload", func(t *testing.T) {
		t.Run("vault", func(t *testing.T) {
			a, _ := newPSPWorld(t)
			_, err := a.alphaRemote.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{DisplayName: ptr("Alpha")})
			require.NoError(t, err)
			written := objectHistory(t, a.alphaRemote, billing.LogObjectMerchant, a.alpha.MerchantID().String())[0]
			require.Equal(t, billing.LogSourceAPI, written.Source)
			path := a.cfg.Vault.KVMount + "/data/openrails/merchants/" + a.alpha.MerchantID().UUID().String() + "/merchant"
			held, err := a.deps.Vault.Logical().ReadWithContext(t.Context(), path)
			require.NoError(t, err)
			data := held.Data["data"].(map[string]any)
			data["display_name"] = "Alpha Outside"
			_, err = a.deps.Vault.Logical().WriteWithContext(t.Context(), path, map[string]any{"data": data})
			require.NoError(t, err)

			loadInReplicas(t, a.cfg, a.deps, a.alpha.MerchantID(), 2)
			var outside []billing.LogEntry
			for _, e := range objectHistory(t, a.alphaRemote, billing.LogObjectMerchant, a.alpha.MerchantID().String()) {
				if e.Source == billing.LogSourceReload {
					outside = append(outside, e)
				}
			}
			require.Len(t, outside, 1, "two replicas loading one Vault edit record it once")
			require.Equal(t, billing.LogActorVault, outside[0].Actor)
			require.Equal(t, billing.LogActionUpdate, outside[0].Action)
			before, after := fieldSides(t, changeOf(t, outside[0]), "display_name")
			require.Equal(t, "Alpha", before)
			require.Equal(t, "Alpha Outside", after)
			require.EqualValues(t, *changeOf(t, written).Revision+1, *changeOf(t, outside[0]).Revision, "the entry names the Vault version it records")

			loadInReplicas(t, a.cfg, a.deps, a.alpha.MerchantID(), 1)
			again := 0
			for _, e := range objectHistory(t, a.alphaRemote, billing.LogObjectMerchant, a.alpha.MerchantID().String()) {
				if e.Source == billing.LogSourceReload {
					again++
				}
			}
			require.Equal(t, 1, again, "a later load records nothing new")
		})
		t.Run("file", func(t *testing.T) {
			w := newWorld(t)
			id := w.alpha.MerchantID().String()
			booted := objectHistory(t, w.alpha, billing.LogObjectMerchant, id)
			require.Len(t, booted, 1)
			require.Equal(t, billing.LogActionCreate, booted[0].Action)
			require.Equal(t, billing.LogActorFile, booted[0].Actor)
			require.Equal(t, billing.LogSourceReload, booted[0].Source)
			_, name := fieldSides(t, changeOf(t, booted[0]), "display_name")
			require.Equal(t, "Alpha", name)

			edited := w.cfg
			edited.Merchants = append([]billing.MerchantDeclaration(nil), w.cfg.Merchants...)
			alpha := cfgMerchant(edited, "alpha")
			alpha.DisplayName = "Alpha Two"
			setCfgMerchant(&edited, "alpha", alpha)
			restart(t, edited, w.deps)
			restart(t, edited, w.deps)
			entries := objectHistory(t, w.alpha, billing.LogObjectMerchant, id)
			require.Equal(t, []billing.LogAction{billing.LogActionUpdate, billing.LogActionCreate}, actions(entries), "the edited file is recorded once, however many processes start on it")
			require.Equal(t, billing.LogActorFile, entries[0].Actor)
			before, after := fieldSides(t, changeOf(t, entries[0]), "display_name")
			require.Equal(t, "Alpha", before)
			require.Equal(t, "Alpha Two", after)
			require.EqualValues(t, 0, *changeOf(t, entries[0]).Revision)
		})
	})

	t.Run("OR-J43-S05/the-list-filters-by-object-and-time-and-pages-by-cursor", func(t *testing.T) {
		a := newAuthorityWorld(t)
		start := a.clock.Now().UTC().Truncate(time.Microsecond)
		_, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "first", DisplayName: "First"})
		require.NoError(t, err)
		a.clock.Advance(time.Hour)
		second, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "second", DisplayName: "Second"})
		require.NoError(t, err)
		a.clock.Advance(time.Hour)
		_, err = a.alphaRemote.UpdateProduct(t.Context(), second.ID, billing.UpdateProductParams{DisplayName: catalog.Field[string]{Set: true, Value: "Second edition"}})
		require.NoError(t, err)
		_, err = a.betaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "second", DisplayName: "Beta's second"})
		require.NoError(t, err)

		all := historyOf(t, a.alphaRemote, billing.LogListParams{})
		require.Len(t, all, 3, "a merchant reads its own history only")
		require.Equal(t, []billing.LogAction{billing.LogActionUpdate, billing.LogActionCreate}, actions(objectHistory(t, a.alphaRemote, billing.LogObjectProduct, "second")))
		require.Empty(t, objectHistory(t, a.alphaRemote, billing.LogObjectPrice, "second"))
		since := historyOf(t, a.alphaRemote, billing.LogListParams{Since: start.Add(30 * time.Minute)})
		require.Len(t, since, 2)
		until := historyOf(t, a.alphaRemote, billing.LogListParams{Until: start.Add(30 * time.Minute)})
		require.Len(t, until, 1)
		require.Equal(t, "first", until[0].ObjectID)
		window := historyOf(t, a.alphaRemote, billing.LogListParams{Since: start.Add(30 * time.Minute), Until: start.Add(90 * time.Minute)})
		require.Len(t, window, 1)
		require.Equal(t, billing.LogActionCreate, window[0].Action)

		paged := historyOf(t, a.alphaRemote, billing.LogListParams{PageRequest: billing.PageRequest{Limit: 1}})
		require.Equal(t, all, paged, "one entry a page visits each once, newest first")
		for i := 1; i < len(all); i++ {
			require.False(t, all[i].OccurredAt.After(all[i-1].OccurredAt))
		}

		_, err = a.alphaRemote.ListChanges(t.Context(), billing.LogListParams{ObjectID: "second"})
		requireCode(t, err, billing.CodeInvalidParam)
		_, err = a.alphaRemote.ListChanges(t.Context(), billing.LogListParams{ObjectType: "subscription"})
		requireCode(t, err, billing.CodeInvalidParam)
		reader, err := a.auth.CreateAPIKey(t.Context(), iam.SystemIdentity(), a.alphaGroup, iam.NewAPIKey{Name: "customer-reader", Role: a.reader})
		require.NoError(t, err)
		list := "/v1/merchant/changes?" + url.Values{"object_type": {"product"}}.Encode()
		for _, check := range []struct {
			token  string
			status int
		}{{"", 401}, {a.aliceToken, 403}, {reader.Secret, 403}, {a.alphaKey.Secret, 200}} {
			status, raw := a.request(t, http.MethodGet, list, check.token, nil)
			require.Equal(t, check.status, status, string(raw))
		}
	})

	t.Run("OR-J43-S06/a-products-revision-moves-with-its-keys-not-its-prices-and-its-entries-carry-it", func(t *testing.T) {
		a := newAuthorityWorld(t)
		show, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "show", DisplayName: "Show", Entitlements: []string{"sd"}})
		require.NoError(t, err)
		read, err := a.alphaRemote.GetProduct(t.Context(), show.ID)
		require.NoError(t, err)
		require.Equal(t, show.Revision, read.Revision)
		require.Positive(t, read.Revision)
		price, err := a.alphaRemote.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: show.ID, Key: "once", UnitAmount: 3_000_000, Currency: "USD"})
		require.NoError(t, err)
		_, err = a.alphaRemote.UpdatePrice(t.Context(), price.ID, billing.UpdatePriceParams{Archived: catalog.Field[bool]{Set: true, Value: true}})
		require.NoError(t, err)
		read, err = a.alphaRemote.GetProduct(t.Context(), show.ID)
		require.NoError(t, err)
		require.Equal(t, show.Revision, read.Revision, "a price change never moves the product's revision")
		listed, err := a.alphaRemote.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{"show"}})
		require.NoError(t, err)
		require.Len(t, listed.Items, 1)
		require.Equal(t, show.Revision, listed.Items[0].Revision)

		keyed, err := a.alphaRemote.UpdateProduct(t.Context(), show.ID, billing.UpdateProductParams{Entitlements: catalog.Field[[]string]{Set: true, Value: []string{"sd", "hd"}}})
		require.NoError(t, err)
		require.Greater(t, keyed.Revision, show.Revision, "a key change moves it")
		entries := objectHistory(t, a.alphaRemote, billing.LogObjectProduct, "show")
		require.Equal(t, []billing.LogAction{billing.LogActionKeyAdd, billing.LogActionKeyAdd, billing.LogActionCreate}, actions(entries))
		added := changeOf(t, entries[0])
		require.Equal(t, []string{"hd"}, added.Keys)
		require.EqualValues(t, keyed.Revision, *added.Revision, "revision %d added hd", keyed.Revision)
		require.EqualValues(t, show.Revision, *changeOf(t, entries[2]).Revision)
		prices := objectHistory(t, a.alphaRemote, billing.LogObjectPrice, "show:once")
		require.Equal(t, []billing.LogAction{billing.LogActionArchive, billing.LogActionCreate}, actions(prices))
		archived, err := a.alphaRemote.GetPrice(t.Context(), price.ID)
		require.NoError(t, err)
		require.EqualValues(t, archived.Revision, *changeOf(t, prices[0]).Revision, "a price's entry carries its key's revision")
	})

	t.Run("OR-J43-S07/a-stale-expected-revision-is-refused-and-the-current-one-succeeds", func(t *testing.T) {
		a := newAuthorityWorld(t)
		p, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "tracked", DisplayName: "Tracked", Entitlements: []string{"sd"}})
		require.NoError(t, err)
		stale := p.Revision
		moved, err := a.alphaRemote.UpdateProduct(t.Context(), p.ID, billing.UpdateProductParams{Entitlements: catalog.Field[[]string]{Set: true, Value: []string{"sd", "hd"}}, ExpectedRevision: &stale})
		require.NoError(t, err)
		current := moved.Revision
		require.Greater(t, current, stale)
		recorded := len(historyOf(t, a.alphaRemote, billing.LogListParams{}))
		for name, edit := range map[string]func() error{
			"rename": func() error {
				_, err := a.alphaRemote.UpdateProduct(t.Context(), p.ID, billing.UpdateProductParams{DisplayName: catalog.Field[string]{Set: true, Value: "Overwritten"}, ExpectedRevision: &stale})
				return err
			},
			"archive": func() error {
				_, err := a.alphaRemote.UpdateProduct(t.Context(), p.ID, billing.UpdateProductParams{Archived: catalog.Field[bool]{Set: true, Value: true}, ExpectedRevision: &stale})
				return err
			},
			"keys": func() error {
				_, err := a.alphaRemote.UpdateProduct(t.Context(), p.ID, billing.UpdateProductParams{Entitlements: catalog.Field[[]string]{Set: true, Value: []string{"sd"}}, ExpectedRevision: &stale})
				return err
			},
			"delete": func() error {
				return a.alphaRemote.DeleteProduct(t.Context(), p.ID, billing.DeleteProductParams{ExpectedRevision: &stale})
			},
		} {
			failure := requireCode(t, edit(), billing.CodeRevisionMismatch)
			require.Equal(t, http.StatusConflict, failure.Status, name)
			require.EqualValues(t, current, failure.Metadata["revision"], "%s names the current revision", name)
		}
		read, err := a.alphaRemote.GetProduct(t.Context(), p.ID)
		require.NoError(t, err)
		require.Equal(t, "Tracked", read.DisplayName)
		require.False(t, read.Archived)
		require.ElementsMatch(t, []string{"sd", "hd"}, read.Entitlements)
		require.Equal(t, current, read.Revision)
		require.Len(t, historyOf(t, a.alphaRemote, billing.LogListParams{}), recorded, "a refused edit records nothing")

		renamed, err := a.alphaRemote.UpdateProduct(t.Context(), p.ID, billing.UpdateProductParams{DisplayName: catalog.Field[string]{Set: true, Value: "Tracked again"}, ExpectedRevision: &current})
		require.NoError(t, err)
		require.Equal(t, "Tracked again", renamed.DisplayName)
		next := renamed.Revision
		require.NoError(t, a.alphaRemote.DeleteProduct(t.Context(), p.ID, billing.DeleteProductParams{ExpectedRevision: &next}))
		_, err = a.alphaRemote.GetProduct(t.Context(), p.ID)
		require.ErrorIs(t, err, billing.ErrNotFound)
		entries := objectHistory(t, a.alphaRemote, billing.LogObjectProduct, "tracked")
		require.Equal(t, []billing.LogAction{billing.LogActionDelete, billing.LogActionKeyRemove, billing.LogActionUpdate}, actions(entries[:3]))
		require.ElementsMatch(t, []string{"sd", "hd"}, changeOf(t, entries[1]).Keys)
	})
}

// loadInReplicas starts n processes on cfg, has them all load merchant's
// configuration at once, and closes them.
func loadInReplicas(t *testing.T, cfg openrails.Config, deps openrails.Deps, merchant billing.MerchantID, n int) {
	t.Helper()
	clients := make([]*openrails.Client, n)
	for i := range clients {
		replica, err := openrails.New(t.Context(), cfg, deps)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, replica.Close(context.Background())) })
		clients[i], err = replica.Merchant(merchant)
		require.NoError(t, err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for _, client := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.GetMerchantConfiguration(t.Context())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

// restart starts a process on cfg, as a deploy of an edited file does: it
// loads its merchants on start.
func restart(t *testing.T, cfg openrails.Config, deps openrails.Deps) {
	t.Helper()
	engine, err := openrails.New(t.Context(), cfg, deps)
	require.NoError(t, err)
	require.NoError(t, engine.Close(context.Background()))
}
