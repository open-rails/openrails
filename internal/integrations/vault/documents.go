package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
)

// Document is one KV v2 document: a JSON object and its version.
type Document struct {
	Data json.RawMessage
	// Version is the path's current version; 0 when it was never written.
	Version int64
	// Updated is when Version was written.
	Updated time.Time
	// Found is false when the path holds no data: never written, or its
	// current version deleted.
	Found bool
}

// ErrCASMismatch refuses a write whose check-and-set version is not the
// path's current version.
var ErrCASMismatch = errors.New("vault: check-and-set version mismatch")

func (a *KVv2Adapter) documentPath(segment, rel string) string {
	return a.mount + "/" + segment + "/" + strings.Trim(rel, "/")
}

// ReadDocument reads the current version of the document at rel, a path
// under the mount.
func (a *KVv2Adapter) ReadDocument(ctx context.Context, rel string) (Document, error) {
	if err := a.sup.AuthState(); err != nil {
		return Document{}, fmt.Errorf("vault kv read: %w", err)
	}
	sec, err := a.client.Logical().ReadWithDataWithContext(ctx, a.documentPath("data", rel), nil)
	if err != nil {
		a.notifyErr(err)
		return Document{}, fmt.Errorf("vault kv read %s: %w", rel, credentialBackendError(err))
	}
	if sec == nil || sec.Data == nil {
		return Document{}, nil
	}
	doc := documentMetadata(sec.Data["metadata"])
	if inner, ok := sec.Data["data"].(map[string]any); ok && inner != nil {
		raw, err := json.Marshal(inner)
		if err != nil {
			return Document{}, fmt.Errorf("vault kv read %s: %w", rel, err)
		}
		doc.Data, doc.Found = raw, true
	}
	return doc, nil
}

// WriteDocument writes data, a JSON object, at rel when cas is the path's
// current version (0 creates the path), and returns the new version.
func (a *KVv2Adapter) WriteDocument(ctx context.Context, rel string, data json.RawMessage, cas int64) (Document, error) {
	if cas < 0 {
		return Document{}, fmt.Errorf("vault kv: invalid check-and-set version")
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return Document{}, fmt.Errorf("vault kv write %s: the document must be a JSON object", rel)
	}
	if err := a.sup.AuthState(); err != nil {
		return Document{}, fmt.Errorf("vault kv write: %w", err)
	}
	body := map[string]any{"data": object, "options": map[string]any{"cas": cas}}
	sec, err := a.client.Logical().WriteWithContext(ctx, a.documentPath("data", rel), body)
	if err != nil {
		a.notifyErr(err)
		var response *vaultapi.ResponseError
		if errors.As(err, &response) && response.StatusCode == 400 {
			for _, message := range response.Errors {
				if strings.Contains(message, "check-and-set") {
					return Document{}, ErrCASMismatch
				}
			}
			return Document{}, fmt.Errorf("vault kv write %s: %w", rel, err)
		}
		return Document{}, fmt.Errorf("vault kv write %s: %w", rel, credentialBackendError(err))
	}
	doc := Document{Data: data, Found: true}
	if sec != nil {
		meta := documentMetadata(sec.Data)
		doc.Version, doc.Updated = meta.Version, meta.Updated
	}
	if doc.Version == 0 {
		return Document{}, fmt.Errorf("vault kv write %s: the mount answered no version; is it KV v2?", rel)
	}
	return doc, nil
}

// ListDocuments lists the names one level under rel; a name ending in "/"
// holds more. A path holding nothing lists nothing.
func (a *KVv2Adapter) ListDocuments(ctx context.Context, rel string) ([]string, error) {
	if err := a.sup.AuthState(); err != nil {
		return nil, fmt.Errorf("vault kv list: %w", err)
	}
	sec, err := a.client.Logical().ListWithContext(ctx, a.documentPath("metadata", rel))
	if err != nil {
		a.notifyErr(err)
		return nil, fmt.Errorf("vault kv list %s: %w", rel, credentialBackendError(err))
	}
	if sec == nil || sec.Data == nil {
		return nil, nil
	}
	raw, _ := sec.Data["keys"].([]any)
	out := make([]string, 0, len(raw))
	for _, k := range raw {
		if s, ok := k.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// DeleteDocument removes every version of the document at rel.
func (a *KVv2Adapter) DeleteDocument(ctx context.Context, rel string) error {
	if err := a.sup.AuthState(); err != nil {
		return fmt.Errorf("vault kv delete: %w", err)
	}
	if _, err := a.client.Logical().DeleteWithContext(ctx, a.documentPath("metadata", rel)); err != nil {
		a.notifyErr(err)
		return fmt.Errorf("vault kv delete %s: %w", rel, credentialBackendError(err))
	}
	return nil
}

func documentMetadata(raw any) Document {
	meta, _ := raw.(map[string]any)
	var doc Document
	if meta == nil {
		return doc
	}
	if n, ok := meta["version"].(json.Number); ok {
		doc.Version, _ = n.Int64()
	}
	if s, ok := meta["created_time"].(string); ok {
		doc.Updated, _ = time.Parse(time.RFC3339Nano, s)
	}
	return doc
}
