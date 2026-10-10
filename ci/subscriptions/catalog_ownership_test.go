//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/service"
)

const catalogConflictsFinding = string(service.FindingCatalogConflicts)

func usd(amount int64) catalog.ApplyPrice {
	return catalog.ApplyPrice{Currency: catalog.Value("USD"), UnitAmount: catalog.Value(amount)}
}

func catalogDocument(products map[string]catalog.ApplyProduct) *catalog.Application {
	return &catalog.Application{SchemaVersion: catalog.ApplicationSchemaVersion, Products: products}
}

// fieldOwners lists the managers that hold one field of a catalog object.
func (w *world) fieldOwners(object, key, priceKey, field string) []string {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), `SELECT manager FROM `+pgx.Identifier{w.schema}.Sanitize()+`.catalog_field_owners
		WHERE object = $1 AND key = $2 AND price_key = $3 AND field = $4 ORDER BY manager`, object, key, priceKey, field)
	require.NoError(w.t, err)
	managers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(w.t, err)
	if managers == nil {
		managers = []string{}
	}
	return managers
}

func (w *world) amountOf(product, price string) int64 {
	w.t.Helper()
	p, err := priceByKey(w.t.Context(), w.client[embedded], product, price)
	require.NoError(w.t, err)
	return p.UnitAmount
}

// evidenceLines is a finding's readable conflict lines.
func evidenceLines(t *testing.T, f billing.Finding) string {
	t.Helper()
	raw, err := json.Marshal(f.Evidence["conflicts"])
	require.NoError(t, err)
	var lines []string
	require.NoError(t, json.Unmarshal(raw, &lines))
	return strings.Join(lines, "\n")
}

func changed(receipt *billing.CatalogApplicationReceipt, object billing.CatalogObjectKind, key string) *billing.CatalogChange {
	for i, c := range receipt.Changes {
		if c.Object == object && c.Key == key {
			return &receipt.Changes[i]
		}
	}
	return nil
}

// The owner's example: the file sets A $1 and B $2; staff set A $1.50; the
// file changes B to $2.50. B applies, A stays $1.50 and is reported, and the
// finding stays open until the file agrees.
func TestCatalogDocumentSkipsWhatAnEditChanged(t *testing.T) {
	w := newWorld(t)
	c := w.client[embedded]
	key := "plans-" + uuid.NewString()[:8]
	file := func(a, b int64) *catalog.Application {
		return catalogDocument(map[string]catalog.ApplyProduct{key: {DisplayName: catalog.Value("Plans"), Prices: map[string]catalog.ApplyPrice{"a": usd(a), "b": usd(b)}}})
	}
	first, err := c.ApplyCatalog(t.Context(), file(1_000_000, 2_000_000), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	require.Empty(t, first.Conflicts)
	require.Equal(t, []string{"apply"}, w.fieldOwners("price", key, "a", "unit_amount"))
	product, err := productByKey(t.Context(), c, key)
	require.NoError(t, err)
	edited, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "a", Currency: "USD", UnitAmount: 1_500_000})
	require.NoError(t, err)
	require.Equal(t, []string{"edit"}, w.fieldOwners("price", key, "a", "unit_amount"), "the edit took the field from the file")

	// Over HTTP: a 200 that names what it skipped.
	receipt, err := w.client[remote].ApplyCatalog(t.Context(), file(1_000_000, 2_500_000), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	require.False(t, receipt.Replayed)
	require.Len(t, receipt.Conflicts, 1)
	conflict := receipt.Conflicts[0]
	require.Equal(t, billing.CatalogObjectPrice, conflict.Object)
	require.Equal(t, "a", conflict.Key)
	require.Equal(t, key, *conflict.ProductKey)
	require.Equal(t, "USD", *conflict.Currency)
	require.Len(t, conflict.Fields, 1)
	require.Equal(t, "unit_amount", conflict.Fields[0].Field)
	require.JSONEq(t, `"1000000"`, string(conflict.Fields[0].FileValue))
	require.JSONEq(t, `"1500000"`, string(conflict.Fields[0].LiveValue))
	require.Equal(t, "api", conflict.Fields[0].SetBy)
	require.False(t, conflict.Fields[0].SetAt.IsZero())
	require.NotNil(t, changed(receipt, billing.CatalogObjectPrice, "b"))
	require.Nil(t, changed(receipt, billing.CatalogObjectPrice, "a"))
	require.EqualValues(t, 1_500_000, w.amountOf(key, "a"), "nothing an edit set is reverted")
	require.EqualValues(t, 2_500_000, w.amountOf(key, "b"))
	require.Equal(t, []string{"catalog"}, w.openFindings(catalogConflictsFinding))
	findings, err := c.ListFindings(t.Context(), billing.FindingListParams{Type: catalogConflictsFinding})
	require.NoError(t, err)
	require.Len(t, findings.Items, 1)
	require.Contains(t, evidenceLines(t, findings.Items[0]), "price a of product "+key+" skipped: unit_amount: the file says 1.00 USD, an edit set 1.50 USD (api, ")

	// Not recorded: applying it again retries the skipped object; the parts
	// that applied are no-ops, and the finding is one, updated.
	again, err := c.ApplyCatalog(t.Context(), file(1_000_000, 2_500_000), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	require.False(t, again.Replayed)
	require.Len(t, again.Conflicts, 1)
	require.Empty(t, again.Changes)
	require.Equal(t, []string{"catalog"}, w.openFindings(catalogConflictsFinding))

	// The file agrees: it applies whole, shares the field, changes nothing and
	// closes the finding.
	fixed, err := c.ApplyCatalog(t.Context(), file(1_500_000, 2_500_000), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	require.Empty(t, fixed.Conflicts)
	require.Empty(t, fixed.Changes)
	require.Empty(t, w.openFindings(catalogConflictsFinding))
	require.Equal(t, []string{"apply", "edit"}, w.fieldOwners("price", key, "a", "unit_amount"))
	current, err := priceByKey(t.Context(), c, key, "a")
	require.NoError(t, err)
	require.Equal(t, edited.ID, current.ID)
	replay, err := c.ApplyCatalog(t.Context(), file(1_500_000, 2_500_000), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	require.True(t, replay.Replayed)

	// An edit after the apply always succeeds and takes the field.
	_, err = c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "b", Currency: "USD", UnitAmount: 2_750_000})
	require.NoError(t, err)
	require.Equal(t, []string{"edit"}, w.fieldOwners("price", key, "b", "unit_amount"))
	// The applied file is a replay: it never undoes the edit.
	replay, err = c.ApplyCatalog(t.Context(), file(1_500_000, 2_500_000), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.EqualValues(t, 2_750_000, w.amountOf(key, "b"))
}

// A product or a price applies whole: one conflicting field skips all of its
// changes, while every other object of the file applies.
func TestCatalogObjectsApplyWhole(t *testing.T) {
	w := newWorld(t)
	c := w.client[embedded]
	k, l := "k-"+uuid.NewString()[:8], "l-"+uuid.NewString()[:8]
	monthly := func(amount int64, hours int) catalog.ApplyPrice {
		p := usd(amount)
		p.BillingIntervalHours, p.AccessDurationHours = catalog.Value(hours), catalog.Value(hours)
		return p
	}
	_, err := c.ApplyCatalog(t.Context(), catalogDocument(map[string]catalog.ApplyProduct{
		k: {DisplayName: catalog.Value("K"), Description: catalog.Value("first"), Prices: map[string]catalog.ApplyPrice{"m": monthly(1_000_000, 720), "n": usd(2_000_000)}},
		l: {DisplayName: catalog.Value("L")},
	}), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	product, err := productByKey(t.Context(), c, k)
	require.NoError(t, err)
	_, err = c.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Description: catalog.Value("edited")})
	require.NoError(t, err)
	_, err = c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "m", Currency: "USD", UnitAmount: 1_500_000, BillingIntervalHours: new(720), AccessDurationHours: new(720)})
	require.NoError(t, err)

	receipt, err := c.ApplyCatalog(t.Context(), catalogDocument(map[string]catalog.ApplyProduct{
		k: {DisplayName: catalog.Value("K2"), Description: catalog.Value("first"), Prices: map[string]catalog.ApplyPrice{"m": monthly(1_000_000, 8760), "n": usd(3_000_000)}},
		l: {DisplayName: catalog.Value("L2")},
	}), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	require.Len(t, receipt.Conflicts, 2)
	require.Equal(t, billing.CatalogObjectProduct, receipt.Conflicts[0].Object)
	require.Equal(t, k, receipt.Conflicts[0].Key)
	require.Equal(t, "description", receipt.Conflicts[0].Fields[0].Field)
	require.Len(t, receipt.Conflicts[0].Fields, 1, "an equal or unowned field never conflicts")
	require.Equal(t, billing.CatalogObjectPrice, receipt.Conflicts[1].Object)
	require.Equal(t, "m", receipt.Conflicts[1].Key)
	require.Equal(t, "unit_amount", receipt.Conflicts[1].Fields[0].Field)

	product, err = productByKey(t.Context(), c, k)
	require.NoError(t, err)
	require.Equal(t, "K", product.DisplayName, "the product is skipped whole: its unconflicted name did not apply either")
	require.Equal(t, "edited", product.Description)
	other, err := productByKey(t.Context(), c, l)
	require.NoError(t, err)
	require.Equal(t, "L2", other.DisplayName, "another product of the file applies")
	m, err := priceByKey(t.Context(), c, k, "m")
	require.NoError(t, err)
	require.EqualValues(t, 1_500_000, m.UnitAmount)
	require.Equal(t, 720, *m.BillingIntervalHours, "the price is skipped whole: its new interval did not apply either")
	require.EqualValues(t, 3_000_000, w.amountOf(k, "n"), "another price of the skipped product applies")
}

// Removing a field from the file relinquishes it and leaves the live value;
// a file that agrees shares the field; force overwrites and takes it.
func TestCatalogDocumentRelinquishesSharesAndForces(t *testing.T) {
	w := newWorld(t)
	c := w.client[embedded]
	x := "x-" + uuid.NewString()[:8]
	_, err := c.ApplyCatalog(t.Context(), catalogDocument(map[string]catalog.ApplyProduct{x: {DisplayName: catalog.Value("One"), Description: catalog.Value("from the file")}}), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	product, err := productByKey(t.Context(), c, x)
	require.NoError(t, err)
	_, err = c.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{DisplayName: catalog.Value("Edited")})
	require.NoError(t, err)

	// Agreeing shares the field; leaving description out relinquishes it.
	agreed, err := c.ApplyCatalog(t.Context(), catalogDocument(map[string]catalog.ApplyProduct{x: {DisplayName: catalog.Value("Edited")}}), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	require.Empty(t, agreed.Conflicts)
	require.Equal(t, []string{"apply", "edit"}, w.fieldOwners("product", x, "", "display_name"))
	require.Equal(t, []string{}, w.fieldOwners("product", x, "", "description"))
	product, err = productByKey(t.Context(), c, x)
	require.NoError(t, err)
	require.Equal(t, "from the file", product.Description, "relinquishing leaves the live value")

	// Sharing is not owning: a later change of the file conflicts again.
	refused, err := c.ApplyCatalog(t.Context(), catalogDocument(map[string]catalog.ApplyProduct{x: {DisplayName: catalog.Value("Two")}}), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	require.Len(t, refused.Conflicts, 1)
	// Dropping the entry from the file clears the conflict.
	dropped, err := c.ApplyCatalog(t.Context(), catalogDocument(map[string]catalog.ApplyProduct{"other-" + x: {DisplayName: catalog.Value("Other")}}), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	require.Empty(t, dropped.Conflicts)
	require.Empty(t, w.openFindings(catalogConflictsFinding))

	forced, err := c.ApplyCatalog(t.Context(), catalogDocument(map[string]catalog.ApplyProduct{x: {DisplayName: catalog.Value("Two")}}), billing.ApplyCatalogParams{Force: true})
	require.NoError(t, err)
	require.Empty(t, forced.Conflicts)
	require.Equal(t, []string{"display_name"}, changed(forced, billing.CatalogObjectProduct, x).Fields)
	require.Equal(t, []string{"apply"}, w.fieldOwners("product", x, "", "display_name"), "force takes the field")
	product, err = productByKey(t.Context(), c, x)
	require.NoError(t, err)
	require.Equal(t, "Two", product.DisplayName)
}

// prune archives only the omitted objects the file alone set; an omitted
// object an edit touched or created stays, released by the file.
func TestCatalogPruneArchivesOnlyWhatTheFileAloneSet(t *testing.T) {
	w := newWorld(t)
	c := w.client[embedded]
	id := uuid.NewString()[:8]
	p, q, r, s := "p-"+id, "q-"+id, "r-"+id, "s-"+id
	whole := func(products map[string]catalog.ApplyProduct) *catalog.Application {
		doc := catalogDocument(products)
		doc.Prune = true
		return doc
	}
	_, err := c.ApplyCatalog(t.Context(), whole(map[string]catalog.ApplyProduct{
		p: {DisplayName: catalog.Value("P"), Prices: map[string]catalog.ApplyPrice{"one": usd(1_000_000)}},
		q: {DisplayName: catalog.Value("Q"), Prices: map[string]catalog.ApplyPrice{"one": usd(1_000_000)}},
	}), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	qp, err := productByKey(t.Context(), c, q)
	require.NoError(t, err)
	_, err = c.UpdateProduct(t.Context(), qp.ID, billing.UpdateProductParams{Description: catalog.Value("staff note")})
	require.NoError(t, err)
	rp, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: r, DisplayName: "R"})
	require.NoError(t, err)
	_, err = c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: rp.ID, Key: "one", Currency: "USD", UnitAmount: 1_000_000})
	require.NoError(t, err)

	receipt, err := c.ApplyCatalog(t.Context(), whole(map[string]catalog.ApplyProduct{s: {DisplayName: catalog.Value("S")}}), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	require.Empty(t, receipt.Conflicts)
	archived := func(key string) bool {
		product, err := productByKey(t.Context(), c, key)
		require.NoError(t, err)
		return product.Archived
	}
	require.True(t, archived(p), "the file alone set p")
	require.False(t, archived(q), "an edit touched q")
	require.False(t, archived(r), "an edit created r")
	require.Equal(t, []string{}, w.fieldOwners("product", q, "", "display_name"), "the file released what it omitted and could not archive")
	priceArchived := func(product string) bool {
		_, err := priceByKey(t.Context(), c, product, "one")
		if errors.Is(err, billing.ErrNotFound) {
			return true
		}
		require.NoError(t, err)
		return false
	}
	require.True(t, priceArchived(p))
	require.True(t, priceArchived(q), "q's price is its own object, and only the file set it")
	require.False(t, priceArchived(r))
}

// A rename across products lands on each product no edit contests; the result
// names the skipped one and the finding does too.
func TestCatalogRenameAcrossProductsLandsPartly(t *testing.T) {
	w := newWorld(t)
	c := w.client[embedded]
	id := uuid.NewString()[:8]
	x, y := "x-"+id, "y-"+id
	gold, premium := "gold-"+id, "premium-"+id
	_, err := c.ApplyCatalog(t.Context(), catalogDocument(map[string]catalog.ApplyProduct{
		x: {DisplayName: catalog.Value("X"), Entitlements: catalog.Value([]string{gold})},
		y: {DisplayName: catalog.Value("Y"), Entitlements: catalog.Value([]string{gold})},
	}), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	yp, err := productByKey(t.Context(), c, y)
	require.NoError(t, err)
	_, err = c.UpdateProduct(t.Context(), yp.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{gold, "vip-" + id})})
	require.NoError(t, err)

	receipt, err := c.ApplyCatalog(t.Context(), catalogDocument(map[string]catalog.ApplyProduct{
		x: {Entitlements: catalog.Value([]string{premium})},
		y: {Entitlements: catalog.Value([]string{premium})},
	}), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	require.Equal(t, []string{"entitlements"}, changed(receipt, billing.CatalogObjectProduct, x).Fields)
	require.Len(t, receipt.EntitlementChanges, 1)
	require.Equal(t, x, receipt.EntitlementChanges[0].ProductKey)
	require.Len(t, receipt.Conflicts, 1)
	require.Equal(t, y, receipt.Conflicts[0].Key)
	require.Equal(t, "entitlements", receipt.Conflicts[0].Fields[0].Field)
	xp, err := productByKey(t.Context(), c, x)
	require.NoError(t, err)
	require.Equal(t, []string{premium}, xp.Entitlements)
	yp, err = productByKey(t.Context(), c, y)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{gold, "vip-" + id}, yp.Entitlements)
	findings, err := c.ListFindings(t.Context(), billing.FindingListParams{Type: catalogConflictsFinding})
	require.NoError(t, err)
	require.Len(t, findings.Items, 1)
	require.Contains(t, evidenceLines(t, findings.Items[0]), "product "+y+" skipped: entitlements: the file says [\""+premium+"\"], an edit set [\""+gold+"\",\"vip-"+id+"\"] (api, ")
}

// A document's entitlement_replacements are an operator's rename: they move
// the key on every product, edited or not, and leave its owners as they were,
// so a later file still cannot silently undo the edit.
func TestCatalogReplacementsRenameEditedProducts(t *testing.T) {
	w := newWorld(t)
	c := w.client[embedded]
	id := uuid.NewString()[:8]
	x, y := "x-"+id, "y-"+id
	gold, premium, vip := "gold-"+id, "premium-"+id, "vip-"+id
	_, err := c.ApplyCatalog(t.Context(), catalogDocument(map[string]catalog.ApplyProduct{x: {DisplayName: catalog.Value("X"), Entitlements: catalog.Value([]string{gold})}}), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	edited, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: y, DisplayName: "Y", Entitlements: []string{gold, vip}})
	require.NoError(t, err)

	renamed, err := c.ApplyCatalog(t.Context(), &catalog.Application{SchemaVersion: catalog.ApplicationSchemaVersion,
		EntitlementReplacements: []catalog.EntitlementReplacement{{From: gold, To: premium}}}, billing.ApplyCatalogParams{})
	require.NoError(t, err)
	require.Empty(t, renamed.Conflicts)
	require.Len(t, renamed.EntitlementChanges, 2)
	require.Equal(t, []string{"entitlements"}, changed(renamed, billing.CatalogObjectProduct, y).Fields)
	got, err := c.GetProduct(t.Context(), edited.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{premium, vip}, got.Entitlements)
	require.Equal(t, []string{"edit"}, w.fieldOwners("product", y, "", "entitlements"), "the rename takes nothing from the edit")
	require.Equal(t, []string{"apply"}, w.fieldOwners("product", x, "", "entitlements"))

	dropped, err := c.ApplyCatalog(t.Context(), catalogDocument(map[string]catalog.ApplyProduct{y: {Entitlements: catalog.Value([]string{premium})}}), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	require.Len(t, dropped.Conflicts, 1, "the edit still holds the keys it set")
	got, err = c.GetProduct(t.Context(), edited.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{premium, vip}, got.Entitlements)
}

// Every start applies Config.Catalog's uncontested objects; New never fails
// on a conflict, the finding names it and closes once the file agrees.
func TestDeclaredCatalogSkipsEditsOnEveryStart(t *testing.T) {
	w := prepareWorld(t, 12)
	key := "boot-" + uuid.NewString()[:8]
	a, b := int64(1_000_000), int64(2_000_000)
	w.cfg = func(cfg *config.Config) {
		cfg.Catalog = catalogDocument(map[string]catalog.ApplyProduct{key: {DisplayName: catalog.Value("Boot"), Prices: map[string]catalog.ApplyPrice{"a": usd(a), "b": usd(b)}}})
	}
	w.start()
	product, err := productByKey(t.Context(), w.client[embedded], key)
	require.NoError(t, err)
	_, err = w.client[embedded].CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "a", Currency: "USD", UnitAmount: 1_500_000})
	require.NoError(t, err)

	revision := w.catalogRevision()
	w.restart()
	require.Equal(t, revision, w.catalogRevision(), "an applied file replays after an edit")
	require.Empty(t, w.openFindings(catalogConflictsFinding))

	b = 2_500_000
	w.restart()
	require.EqualValues(t, 1_500_000, w.amountOf(key, "a"), "New succeeded and kept the edit")
	require.EqualValues(t, 2_500_000, w.amountOf(key, "b"))
	require.Equal(t, []string{"catalog"}, w.openFindings(catalogConflictsFinding))
	revision = w.catalogRevision()
	w.restart()
	require.Equal(t, revision, w.catalogRevision(), "a retried start that changes nothing does not move the catalog")
	require.Equal(t, []string{"catalog"}, w.openFindings(catalogConflictsFinding), "one finding, not one per start")

	a = 1_500_000
	w.restart()
	require.Empty(t, w.openFindings(catalogConflictsFinding), "the finding closes once the file agrees")
	require.EqualValues(t, 1_500_000, w.amountOf(key, "a"))
}

// Every catalog object carries a revision. An edit sent with a revision the
// object moved past is refused; one sent without applies.
func TestCatalogEditsCarryRevisions(t *testing.T) {
	w := newWorld(t)
	c := w.client[embedded]
	id := uuid.NewString()[:8]
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "rev-" + id, DisplayName: "Rev"})
	require.NoError(t, err)
	require.EqualValues(t, 1, product.Revision)

	// Two staff edit from revision 1 at once: one lands, the other is refused.
	results := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			_, results[i] = c.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{DisplayName: catalog.Value("Edit " + string(rune('A'+i))), ExpectedRevision: new(int64(1))})
		})
	}
	wg.Wait()
	var refused *billing.StatusError
	if results[0] == nil {
		require.ErrorAs(t, results[1], &refused)
	} else {
		require.NoError(t, results[1])
		require.ErrorAs(t, results[0], &refused)
	}
	require.Equal(t, http.StatusConflict, refused.Status)
	require.Equal(t, billing.CodeRevisionMismatch, refused.Code)
	require.Equal(t, "2", fmt.Sprint(refused.Metadata["revision"]), "the refusal carries the current revision")

	// Over HTTP the refusal carries the current revision.
	var body map[string]any
	status := w.staffCall(http.MethodPatch, "/v1/admin/catalog/products/"+product.ID.String(), map[string]any{"display_name": "Stale", "expected_revision": 1}, &body)
	require.Equal(t, http.StatusConflict, status)
	code, _ := errorOf(body)
	require.Equal(t, "revision_mismatch", code)
	require.EqualValues(t, 2, body["error"].(map[string]any)["metadata"].(map[string]any)["revision"])

	// Without the precondition an edit is unconditional.
	unconditional, err := c.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{DisplayName: catalog.Value("Last")})
	require.NoError(t, err)
	require.EqualValues(t, 3, unconditional.Revision)

	// A price key, a meter and an override refuse a stale revision too.
	price, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "one", Currency: "USD", UnitAmount: 1_000_000})
	require.NoError(t, err)
	require.EqualValues(t, 1, price.Revision)
	require.EqualValues(t, 0, price.Version)
	_, err = c.UpdatePrice(t.Context(), price.ID, billing.UpdatePriceParams{Archived: catalog.Value(true), ExpectedRevision: new(int64(7))})
	require.True(t, errors.As(err, &refused) && refused.Code == billing.CodeRevisionMismatch, "%v", err)
	archived, err := c.UpdatePrice(t.Context(), price.ID, billing.UpdatePriceParams{Archived: catalog.Value(true), ExpectedRevision: new(price.Revision)})
	require.NoError(t, err)
	require.Greater(t, archived.Revision, price.Revision)
	meterKey := "events-" + id
	meter, err := c.SetMeter(t.Context(), meterKey, billing.SetMeterParams{Aggregation: catalog.AggregationCount, ExpectedRevision: new(int64(0))})
	require.NoError(t, err)
	require.EqualValues(t, 1, meter.Revision)
	_, err = c.SetMeter(t.Context(), meterKey, billing.SetMeterParams{Aggregation: catalog.AggregationSum, ValueProperty: "units", ExpectedRevision: new(int64(0))})
	require.True(t, errors.As(err, &refused) && refused.Code == billing.CodeRevisionMismatch, "%v", err)

	// A document advances the revisions of the objects it changed only.
	other, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "still-" + id, DisplayName: "Still"})
	require.NoError(t, err)
	receipt, err := c.ApplyCatalog(t.Context(), catalogDocument(map[string]catalog.ApplyProduct{
		product.Key: {DisplayName: catalog.Value("From the file")},
		other.Key:   {DisplayName: catalog.Value("Still")},
	}), billing.ApplyCatalogParams{Force: true})
	require.NoError(t, err)
	after, err := productByKey(t.Context(), c, product.Key)
	require.NoError(t, err)
	require.EqualValues(t, unconditional.Revision+1, after.Revision)
	require.Equal(t, after.Revision, changed(receipt, billing.CatalogObjectProduct, product.Key).Revision)
	untouched, err := productByKey(t.Context(), c, other.Key)
	require.NoError(t, err)
	require.Equal(t, other.Revision, untouched.Revision)
	require.Nil(t, changed(receipt, billing.CatalogObjectProduct, other.Key))
	unchangedPrice, err := c.GetPrice(t.Context(), price.ID, billing.GetPriceParams{})
	require.NoError(t, err)
	require.Equal(t, archived.Revision, unchangedPrice.Revision, "a product change never moves its prices")

	raw, err := json.Marshal(receipt)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"conflicts":[]`)
}
