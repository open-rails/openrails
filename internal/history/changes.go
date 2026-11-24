package history

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/open-rails/openrails/billing"
)

// Version is the payload version every action writes.
const Version = 1

// FieldChange is one field's value before and after; a side the field lacked
// is omitted. A credential reads "changed", never a value.
type FieldChange struct {
	Before json.RawMessage
	After  json.RawMessage
	secret bool
}

// Changed is a credential field that changed: no value is kept.
func Changed() FieldChange { return FieldChange{secret: true} }

// Secret reports a credential's change.
func (f FieldChange) Secret() bool { return f.secret }

const changed = `"changed"`

func (f FieldChange) MarshalJSON() ([]byte, error) {
	if f.secret {
		return []byte(changed), nil
	}
	sides := map[string]json.RawMessage{}
	if f.Before != nil {
		sides["before"] = f.Before
	}
	if f.After != nil {
		sides["after"] = f.After
	}
	return json.Marshal(sides)
}

func (f *FieldChange) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte(changed)) {
		*f = Changed()
		return nil
	}
	var sides struct {
		Before json.RawMessage `json:"before"`
		After  json.RawMessage `json:"after"`
	}
	if err := json.Unmarshal(raw, &sides); err != nil {
		return fmt.Errorf("history: field change: %w", err)
	}
	*f = FieldChange{Before: sides.Before, After: sides.After}
	return nil
}

// Fields is the payload of a create, update, archive, unarchive or delete:
// each changed field and the object's revision after (a product's, a price
// key's, a configuration document's Vault version or 0 from a file; none
// after a delete). A catalog document's application carries its id.
type Fields struct {
	Version       int                    `json:"version"`
	Revision      *int64                 `json:"revision,omitempty"`
	ApplicationID string                 `json:"application_id,omitempty"`
	Fields        map[string]FieldChange `json:"fields"`
}

// Keys is the payload of a key_add or key_remove, with the product's
// revision after.
type Keys struct {
	Version  int      `json:"version"`
	Revision *int64   `json:"revision,omitempty"`
	Keys     []string `json:"keys"`
}

// KeyRename is the payload of a key_rename, with the product's revision
// after.
type KeyRename struct {
	Version  int    `json:"version"`
	Revision *int64 `json:"revision,omitempty"`
	From     string `json:"from"`
	To       string `json:"to"`
}

// KeysAdded is the entry for keys added to product, at revision.
func KeysAdded(product string, revision *int64, keys []string) Entry {
	return Entry{ObjectType: billing.LogObjectProduct, ObjectID: product, Action: billing.LogActionKeyAdd, Changes: Keys{Version: Version, Revision: revision, Keys: keys}}
}

// KeysRemoved is the entry for keys removed from product, at revision.
func KeysRemoved(product string, revision *int64, keys []string) Entry {
	return Entry{ObjectType: billing.LogObjectProduct, ObjectID: product, Action: billing.LogActionKeyRemove, Changes: Keys{Version: Version, Revision: revision, Keys: keys}}
}

// KeyRenamed is the entry for product's key from renamed to, at revision.
func KeyRenamed(product string, revision *int64, from, to string) Entry {
	return Entry{ObjectType: billing.LogObjectProduct, ObjectID: product, Action: billing.LogActionKeyRename, Changes: KeyRename{Version: Version, Revision: revision, From: from, To: to}}
}

// PriceObjectID is a price key's object id: product-key:price-key.
func PriceObjectID(product, price string) string { return product + ":" + price }

// Move is the payload of a price key's move: from the entry's time the key
// named PriceID or, archived, nothing; with the key's revision after. A move
// recorded before configuration history carries no revision.
type Move struct {
	Version  int             `json:"version"`
	Revision *int64          `json:"revision,omitempty"`
	PriceID  billing.PriceID `json:"price_id"`
	Archived bool            `json:"archived"`
}

// PriceMoved is the entry for product's price key moving to price, or, archived,
// to nothing, at revision.
func PriceMoved(product, key string, revision *int64, price billing.PriceID, archived bool) Entry {
	return Entry{ObjectType: billing.LogObjectPrice, ObjectID: PriceObjectID(product, key), Action: billing.LogActionMove,
		Changes: Move{Version: Version, Revision: revision, PriceID: price, Archived: archived}}
}

// ObjectProviderIntent is an operation entry's object: a provider intent, by
// its id.
const ObjectProviderIntent billing.LogObjectType = "provider_intent"

// Operation is the payload of an operation entry: one phase of one attempt
// of a provider intent. Reason is the outcome's, ReasonCode a hold's; both
// and Evidence arrive scrubbed of credentials and card data.
type Operation struct {
	Version        int            `json:"version"`
	Kind           string         `json:"kind,omitempty"`
	Attempt        int32          `json:"attempt"`
	Origin         string         `json:"origin,omitempty"`
	Executor       string         `json:"executor,omitempty"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
	Reason         string         `json:"reason,omitempty"`
	ReasonCode     string         `json:"reason_code,omitempty"`
	Evidence       map[string]any `json:"evidence,omitempty"`
}
