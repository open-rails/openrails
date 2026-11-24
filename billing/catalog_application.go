package billing

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/shared/cadence"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// ApplyCatalogParams is how a catalog document applies. Force overwrites the
// fields an edit set and takes them; without it an object with such a field
// is skipped and reported in Conflicts.
type ApplyCatalogParams struct {
	Force bool `form:"force"`
}

// CatalogApplicationReceipt is the result of applying a catalog document:
// the revisions before and after, and how many products and prices changed.
// Replayed is true when the same document was already applied.
// EntitlementChanges lists how the edit changed existing products' keys;
// Changes lists every object the document changed and Conflicts every object
// it skipped. Warnings lists what it could not do. They are returned once and
// are empty on a replay. A document with conflicts or warnings is not
// recorded as applied: applying it again retries them.
type CatalogApplicationReceipt struct {
	ApplicationID      string              `json:"application_id"`
	BaseRevision       int64               `json:"base_revision"`
	AppliedRevision    int64               `json:"applied_revision"`
	Replayed           bool                `json:"replayed"`
	ProductsChanged    int                 `json:"products_changed"`
	PricesChanged      int                 `json:"prices_changed"`
	EntitlementChanges []EntitlementChange `json:"entitlement_changes"`
	Changes            []CatalogChange     `json:"changes"`
	Conflicts          []CatalogConflict   `json:"conflicts"`
	Warnings           []CatalogWarning    `json:"warnings"`
}

// String is the receipt for a person, as openrails catalog apply prints it:
// the revisions, each object changed and each one skipped, with why.
func (r *CatalogApplicationReceipt) String() string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	if r.Replayed {
		fmt.Fprintf(&b, "%s was already applied; nothing changed\n", r.ApplicationID)
	} else {
		fmt.Fprintf(&b, "%s: catalog revision %d -> %d\n", r.ApplicationID, r.BaseRevision, r.AppliedRevision)
	}
	for _, c := range r.Changes {
		fmt.Fprintf(&b, "  changed %s (%s), now revision %d\n", catalogObjectName(c.Object, c.Key, c.ProductKey), strings.Join(c.Fields, ", "), c.Revision)
	}
	for _, c := range r.Conflicts {
		fmt.Fprintf(&b, "  %s\n", c)
	}
	var refused int
	for _, w := range r.Warnings {
		name := catalogObjectName(CatalogObjectPrice, w.Key, &w.ProductKey)
		if w.Refused {
			refused++
			fmt.Fprintf(&b, "  refused %s: %s\n", name, w.Message)
		} else {
			fmt.Fprintf(&b, "  applied %s with warning: %s\n", name, w.Message)
		}
	}
	if len(r.Conflicts) > 0 {
		fmt.Fprintf(&b, "%d object(s) skipped: an edit set the fields above. Change the file to agree, remove those fields, or apply with force.\n", len(r.Conflicts))
	}
	if refused > 0 {
		fmt.Fprintf(&b, "%d price(s) refused: document not recorded as applied. Fix the file and apply again.\n", refused)
	}
	return b.String()
}

// CatalogWarning is something a document could not do for one price: Refused
// when the price was left out (none of its methods can sell it, or its
// provider did not answer for a new provider price), otherwise the price
// applied without it (a Solana plan not published, a provider's terms not
// read and the cached ones kept).
type CatalogWarning struct {
	Key        string `json:"key"`
	ProductKey string `json:"product_key"`
	Refused    bool   `json:"refused"`
	Message    string `json:"message"`
}

// CatalogObjectKind is a catalog object a document declares: a product, one
// of its price keys, a meter or a limit. Each applies whole or not at all.
type CatalogObjectKind string

const (
	CatalogObjectProduct CatalogObjectKind = "product"
	CatalogObjectPrice   CatalogObjectKind = "price"
	CatalogObjectMeter   CatalogObjectKind = "meter"
	CatalogObjectLimit   CatalogObjectKind = "limit"
)

// CatalogChange is one object a document changed: the fields it set and the
// object's revision after. ProductKey names a price's product.
type CatalogChange struct {
	Object     CatalogObjectKind `json:"object"`
	Key        string            `json:"key"`
	ProductKey *string           `json:"product_key"`
	Fields     []string          `json:"fields"`
	Revision   int64             `json:"revision"`
}

// CatalogConflict is one object a document skipped: none of its changes
// applied, because each of Fields names a value an edit set differently.
// Fix the document to agree, drop the fields from it, or apply with force.
type CatalogConflict struct {
	Object     CatalogObjectKind `json:"object"`
	Key        string            `json:"key"`
	ProductKey *string           `json:"product_key"`
	// Currency is a price's live currency, which its money fields are in.
	Currency *catalog.Currency      `json:"currency"`
	Fields   []CatalogFieldConflict `json:"fields"`
}

// String is the skipped object for a person: each field's two values,
// money and durations in readable units, and who set the live one and when.
func (c CatalogConflict) String() string {
	var file, live string
	if c.Currency != nil {
		file, live = string(*c.Currency), string(*c.Currency)
	}
	for _, f := range c.Fields {
		if f.Field == "currency" {
			_ = json.Unmarshal(f.FileValue, &file)
			_ = json.Unmarshal(f.LiveValue, &live)
		}
	}
	fields := make([]string, 0, len(c.Fields))
	for _, f := range c.Fields {
		fields = append(fields, fmt.Sprintf("%s: the file says %s, an edit set %s (%s, %s)", f.Field,
			readableCatalogValue(f.Field, f.FileValue, file), readableCatalogValue(f.Field, f.LiveValue, live), f.SetBy, f.SetAt.UTC().Format(time.RFC3339)))
	}
	return fmt.Sprintf("%s skipped: %s", catalogObjectName(c.Object, c.Key, c.ProductKey), strings.Join(fields, "; "))
}

func catalogObjectName(object CatalogObjectKind, key string, productKey *string) string {
	if productKey != nil {
		return fmt.Sprintf("price %s of product %s", key, *productKey)
	}
	return fmt.Sprintf("%s %s", object, key)
}

func readableCatalogValue(field string, raw json.RawMessage, currency string) string {
	if string(raw) == "null" {
		return "none"
	}
	switch field {
	case "unit_amount", "trial_unit_amount":
		var text string
		if json.Unmarshal(raw, &text) == nil {
			if amount, err := strconv.ParseInt(text, 10, 64); err == nil {
				if _, known := moneyutil.LookupCurrency(currency); known {
					return moneyutil.FormatAmount(amount, currency)
				}
				return fmt.Sprintf("%d micros", amount)
			}
		}
	case "access_duration_hours", "billing_interval_hours", "billing_interval_months", "trial_duration_hours":
		var hours int
		if json.Unmarshal(raw, &hours) == nil {
			return cadence.FormatHours(hours)
		}
	}
	return string(raw)
}

// CatalogFieldConflict is one field a document names whose live value an
// edit set: the document's value, the live one (each as the document writes
// it), and who set the live value and when.
type CatalogFieldConflict struct {
	Field     string          `json:"field"`
	FileValue json.RawMessage `json:"file_value"`
	LiveValue json.RawMessage `json:"live_value"`
	SetBy     string          `json:"set_by"`
	SetAt     time.Time       `json:"set_at"`
}

// EntitlementChange is how one catalog edit changed an existing product's
// keys. Holders of the product gain Added and lose Removed at once, unless
// another product they hold grants a removed key.
type EntitlementChange struct {
	ProductID  ProductID `json:"product_id"`
	ProductKey string    `json:"product_key"`
	Added      []string  `json:"added"`
	Removed    []string  `json:"removed"`
	// Holders is how many customers held the product when it changed: whose
	// access the edit changed.
	Holders int64 `json:"holders"`
}
