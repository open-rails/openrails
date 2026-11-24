package merchants

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/history"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchantdocs"
)

// Configuration history. A write through OpenRails records its change, as
// its actor's, while it holds the merchant's history lock across the
// source write; a load records what history has not, as the file's or
// Vault's: a Vault document's new version once, a file's document whenever
// its fields differ from what history last recorded. The reference is the
// history itself, so every replica agrees and each change is recorded once.
// A credential is compared only with the value the replica held at the
// version history last recorded, and recorded only as "changed".

// RecordConfigWrite is the cache's RecordFunc.
func (s *Service) RecordConfigWrite(ctx context.Context, id billing.MerchantID, name string, before merchantdocs.Set, next any, put func() (int64, error)) error {
	if s == nil || s.pool == nil {
		_, err := put()
		return err
	}
	var putErr error
	wrote := false
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := q.LockConfigurationHistory(ctx, id.UUID()); err != nil {
			return err
		}
		revision, err := put()
		if err != nil {
			putErr = err
			return err
		}
		wrote = true
		recorded, err := recordedConfig(ctx, q, id)
		if err != nil {
			return err
		}
		prior := before.Documents()
		// A change made outside OpenRails that history has not seen yet is
		// recorded first, as the source's.
		if err := s.recordDocuments(ctx, q, id, recorded, nil, prior, []string{name}, s.reloadProvenance()); err != nil {
			return err
		}
		written := map[string]merchantdocs.Document{name: merchantdocs.DocumentOf(name, next, revision)}
		return s.recordDocuments(ctx, q, id, recorded, prior, written, []string{name}, history.ProvenanceAt(ctx, s.now()))
	})
	if putErr != nil {
		return putErr
	}
	if err != nil && wrote {
		// The write stands; a later load records it as the source's.
		s.Log.WarnContext(ctx, "merchant config: recording a configuration change failed; a later load records it", logx.Err(err), "merchant_id", id.String(), "document", name)
		return nil
	}
	return err
}

// RecordConfigLoad records what a load found that history has not:
// previous is the set the replica served before it.
func (s *Service) RecordConfigLoad(ctx context.Context, id billing.MerchantID, previous, loaded merchantdocs.Set) error {
	if s == nil || s.pool == nil {
		return nil
	}
	before, after := previous.Documents(), loaded.Documents()
	if len(before) > 0 && sameDocuments(before, after) {
		return nil
	}
	return s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := q.LockConfigurationHistory(ctx, id.UUID()); err != nil {
			return err
		}
		recorded, err := recordedConfig(ctx, q, id)
		if err != nil {
			return err
		}
		var gone []string
		for name := range recorded {
			if _, ok := after[name]; !ok {
				if _, refused := loaded.Rejected[name]; !refused {
					gone = append(gone, name)
				}
			}
		}
		// A load that began before another replica created a document must
		// not record it deleted: Vault, read again under the lock, decides.
		if len(gone) > 0 && s.config != nil && s.config.Writable() {
			fresh, err := s.config.Source().Load(ctx, id)
			if err != nil {
				return err
			}
			held := fresh.Documents()
			gone = slices.DeleteFunc(gone, func(name string) bool {
				_, ok := held[name]
				_, refused := fresh.Rejected[name]
				return ok || refused
			})
		}
		names := append(slices.Collect(maps.Keys(after)), gone...)
		return s.recordDocuments(ctx, q, id, recorded, before, after, names, s.reloadProvenance())
	})
}

func (s *Service) reloadProvenance() history.Provenance {
	p := history.Provenance{At: s.now().UTC(), Actor: billing.LogActorFile, Source: billing.LogSourceReload}
	if s.config != nil && s.config.Writable() {
		p.Actor = billing.LogActorVault
	}
	return p
}

func sameDocuments(a, b map[string]merchantdocs.Document) bool {
	if len(a) != len(b) {
		return false
	}
	for name, d := range a {
		o, ok := b[name]
		if !ok || o.Revision != d.Revision || !reflect.DeepEqual(o.Fields, d.Fields) || !maps.Equal(o.Secrets, d.Secrets) {
			return false
		}
	}
	return true
}

// recordedDocument is a document as history last recorded it.
type recordedDocument struct {
	revision int64
	fields   map[string]json.RawMessage
}

// recordedConfig folds the merchant's configuration entries, oldest first,
// into each document as history last recorded it.
func recordedConfig(ctx context.Context, q *gen.Queries, id billing.MerchantID) (map[string]*recordedDocument, error) {
	rows, err := q.ListConfigurationChangeLog(ctx, id.UUID())
	if err != nil {
		return nil, err
	}
	out := map[string]*recordedDocument{}
	for _, row := range rows {
		name, err := configDocumentName(billing.LogObjectType(row.ObjectType), row.ObjectID)
		if err != nil {
			return nil, err
		}
		if billing.LogAction(row.Action) == billing.LogActionDelete {
			delete(out, name)
			continue
		}
		var change history.Fields
		if err := json.Unmarshal(row.Changes, &change); err != nil {
			return nil, fmt.Errorf("merchants: configuration history of %s: %w", name, err)
		}
		d := out[name]
		if d == nil || billing.LogAction(row.Action) == billing.LogActionCreate {
			d = &recordedDocument{fields: map[string]json.RawMessage{}}
			out[name] = d
		}
		if change.Revision != nil {
			d.revision = *change.Revision
		}
		for field, c := range change.Fields {
			switch {
			case c.Secret():
			case c.After != nil:
				d.fields[field] = canonical(c.After)
			default:
				delete(d.fields, field)
			}
		}
	}
	return out, nil
}

// recordDocuments records each named document's change from what history
// recorded to after, and updates recorded. before is what the replica held,
// to tell a credential's change; nil when it held nothing to compare.
func (s *Service) recordDocuments(ctx context.Context, q *gen.Queries, id billing.MerchantID, recorded map[string]*recordedDocument, before, after map[string]merchantdocs.Document, names []string, p history.Provenance) error {
	slices.Sort(names)
	var entries []history.Entry
	for _, name := range slices.Compact(names) {
		objectType, objectID := configObject(id, name)
		rec, known := recorded[name]
		cur, present := after[name]
		entry := history.Entry{ObjectType: objectType, ObjectID: objectID, Action: billing.LogActionUpdate}
		fields := map[string]history.FieldChange{}
		switch {
		case !present && !known:
			continue
		case !present:
			entry.Action = billing.LogActionDelete
			for field, v := range rec.fields {
				fields[field] = history.FieldChange{Before: v}
			}
			delete(recorded, name)
			entry.Changes = history.Fields{Version: history.Version, Fields: fields}
			entries = append(entries, entry)
			continue
		case !known:
			entry.Action = billing.LogActionCreate
			rec = &recordedDocument{fields: map[string]json.RawMessage{}}
			for secret := range cur.Secrets {
				fields[secret] = history.Changed()
			}
		}
		for field := range mergedKeys(rec.fields, cur.Fields) {
			was, is := rec.fields[field], canonical(cur.Fields[field])
			if !bytes.Equal(was, is) {
				fields[field] = history.FieldChange{Before: was, After: is}
			}
		}
		if prior, ok := before[name]; ok && known && prior.Revision == rec.revision {
			for secret := range mergedKeys(prior.Secrets, cur.Secrets) {
				if prior.Secrets[secret] != cur.Secrets[secret] {
					fields[secret] = history.Changed()
				}
			}
		}
		// A Vault document is recorded once per new version; an older one is
		// a load that began before the newer was recorded. A file's document
		// is recorded when it differs.
		if known && (cur.Revision > 0 && cur.Revision <= rec.revision || cur.Revision == 0 && len(fields) == 0) {
			continue
		}
		if a, ok := fields["archived"]; ok && known && !a.Secret() {
			entry.Action = billing.LogActionUnarchive
			if string(a.After) == "true" {
				entry.Action = billing.LogActionArchive
			}
		}
		entry.Changes = history.Fields{Version: history.Version, Revision: new(cur.Revision), Fields: fields}
		entries = append(entries, entry)
		fieldsAfter := make(map[string]json.RawMessage, len(cur.Fields))
		for field, v := range cur.Fields {
			fieldsAfter[field] = canonical(v)
		}
		recorded[name] = &recordedDocument{revision: cur.Revision, fields: fieldsAfter}
	}
	if len(entries) == 0 {
		return nil
	}
	return history.Record(ctx, q, history.ChangeLog, id.UUID(), p, entries...)
}

// canonical is a JSON value as Go writes it: jsonb, which the recorded
// values are read back from, spaces and orders them otherwise.
func canonical(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var v any
	if err := decoder.Decode(&v); err != nil {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

func mergedKeys[V any](a, b map[string]V) map[string]bool {
	out := make(map[string]bool, len(a)+len(b))
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// configObject is a configuration document's object in history.
func configObject(id billing.MerchantID, name string) (billing.LogObjectType, string) {
	if name == merchantdocs.MerchantDoc {
		return billing.LogObjectMerchant, id.String()
	}
	psp, custodian := merchantdocs.ParseDoc(name)
	if psp != "" {
		return billing.LogObjectPSP, psp
	}
	return billing.LogObjectCustodian, custodian
}

// configDocumentName is the document a configuration entry is about.
func configDocumentName(objectType billing.LogObjectType, objectID string) (string, error) {
	switch objectType {
	case billing.LogObjectMerchant:
		return merchantdocs.MerchantDoc, nil
	case billing.LogObjectPSP:
		return merchantdocs.PSPDoc(objectID), nil
	case billing.LogObjectCustodian:
		return merchantdocs.CustodianDoc(objectID), nil
	}
	return "", fmt.Errorf("merchants: %s is not a configuration document", objectType)
}
