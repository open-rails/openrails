package merchantdocs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
)

// Document is one configuration document as its history sees it: its
// fields, and its credentials apart, which history never records.
type Document struct {
	Revision int64
	// Fields are the document's fields; secrets lists its credentials'
	// names.
	Fields map[string]json.RawMessage
	// Secrets are its credentials by field name (secrets.<name>, an alert
	// webhook's URL).
	Secrets map[string]string
}

// Documents are the set's documents by name.
func (s Set) Documents() map[string]Document {
	out := map[string]Document{}
	if s.HasMerchant {
		out[MerchantDoc] = DocumentOf(MerchantDoc, s.Merchant.Value, s.Merchant.Revision)
	}
	if s.HasMessages {
		out[MessagesDoc] = DocumentOf(MessagesDoc, s.Messages.Value, s.Messages.Revision)
	}
	for key, d := range s.PSPs {
		out[PSPDoc(key)] = DocumentOf(PSPDoc(key), d.Value, d.Revision)
	}
	for key, d := range s.Custodians {
		out[CustodianDoc(key)] = DocumentOf(CustodianDoc(key), d.Value, d.Revision)
	}
	return out
}

// DocumentOf is the document name holding value at revision.
func DocumentOf(name string, value any, revision int64) Document {
	d := Document{Revision: revision, Fields: map[string]json.RawMessage{}, Secrets: map[string]string{}}
	var fields map[string]any
	var secrets map[string]string
	switch v := value.(type) {
	case Merchant:
		for _, hook := range v.AlertWebhooks {
			field := "alert_webhooks." + hook.ID.String()
			d.Fields[field] = jsonValue(map[string]any{"name": hook.Name, "format": hook.Format, "enabled": hook.Enabled})
			d.Secrets[field+".url"] = hook.URL
		}
		if v.Secrets.SCIMToken != "" {
			secrets = map[string]string{"scim_token": v.Secrets.SCIMToken}
		}
		v.AlertWebhooks, v.Secrets = nil, MerchantSecrets{}
		fields = objectFields(v)
	case billing.MessageSettings:
		// One field per event type and per layout language.
		for event, settings := range v.Events {
			d.Fields["events."+event] = jsonValue(settings)
		}
		for language, layout := range v.Layouts {
			d.Fields["layouts."+language] = jsonValue(layout)
		}
		v.Events, v.Layouts = nil, nil
		fields = objectFields(v)
	case PSP:
		fields, secrets = config.PSPDocument(v).Fields(), v.Secrets
	case Custodian:
		fields, secrets = config.CustodianDocument(v).Fields(), v.Secrets
	default:
		panic(fmt.Sprintf("merchantdocs: %s is not a document (%T)", name, value))
	}
	delete(fields, "secrets")
	for field, value := range fields {
		d.Fields[field] = jsonValue(value)
	}
	for secret, value := range secrets {
		d.Secrets["secrets."+secret] = value
	}
	if len(secrets) > 0 {
		d.Fields["secrets"] = jsonValue(slices.Sorted(maps.Keys(secrets)))
	}
	return d
}

// objectFields are v's JSON object members, numbers kept exact.
func objectFields(v any) map[string]any {
	decoder := json.NewDecoder(bytes.NewReader(jsonValue(v)))
	decoder.UseNumber()
	var fields map[string]any
	if err := decoder.Decode(&fields); err != nil {
		panic(fmt.Sprintf("merchantdocs: document fields: %v", err))
	}
	return fields
}

func jsonValue(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("merchantdocs: document value: %v", err))
	}
	return raw
}

// RecordFunc runs put, which writes document name and answers its new
// revision, and records the change from before in the configuration
// history as ctx's actor's, so that no load records it first. It answers
// put's error as is.
type RecordFunc func(ctx context.Context, id billing.MerchantID, name string, before Set, next any, put func() (int64, error)) error

// SetRecord installs how a write is recorded in the configuration history.
func (c *Cache) SetRecord(f RecordFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record = f
}

// recorded runs put through the history recorder.
func (c *Cache) recorded(ctx context.Context, id billing.MerchantID, name string, before Set, next any, put func() (int64, error)) error {
	c.mu.Lock()
	record := c.record
	c.mu.Unlock()
	if record == nil {
		_, err := put()
		return err
	}
	return record(ctx, id, name, before, next, put)
}
