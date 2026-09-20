package providerqualification

import (
	"encoding/json"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestQualificationBindsOnlyServerCredentialAndItsVersion(t *testing.T) {
	id := uuid.New()
	record := Record{PSPID: id, Environment: "test", Contract: NMIContract, EvidenceRef: "operator-proof"}
	input, err := json.Marshal(map[string]any{"settings": map[string]any{setting: record}})
	require.NoError(t, err)
	row := gen.OpenrailsPsp{ID: id, Rail: "nmi", Environment: "test", Evidence: []byte(`{"credential_versions":{"security_key":7,"webhook_signing_secret":2}}`)}
	bound, err := BindManifest(row, input, func(version int) (string, error) { require.Equal(t, 7, version); return "current credential", nil })
	require.NoError(t, err)
	row.Evidence = bound
	got, err := Current(row)
	require.NoError(t, err)
	require.Equal(t, Fingerprint("current credential"), got.CredentialFingerprint)
	require.Equal(t, 7, *got.CredentialVersion)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(bound, &doc))
	doc["credential_versions"].(map[string]any)["webhook_signing_secret"] = 3
	row.Evidence, err = json.Marshal(doc)
	require.NoError(t, err)
	_, err = Current(row)
	require.NoError(t, err, "an unrelated webhook secret is not payment-account authority")
	doc["credential_versions"].(map[string]any)["security_key"] = 8
	row.Evidence, err = json.Marshal(doc)
	require.NoError(t, err)
	_, err = Current(row)
	require.ErrorIs(t, err, ErrUnqualified)
	row.Evidence = input
	_, err = Current(row)
	require.ErrorIs(t, err, ErrInvalid, "unbound pre-cut records are not grandfathered")
	var caller map[string]any
	require.NoError(t, json.Unmarshal(input, &caller))
	caller["settings"].(map[string]any)[setting].(map[string]any)["credential_fingerprint"] = Fingerprint("forged")
	forged, err := json.Marshal(caller)
	require.NoError(t, err)
	_, err = BindManifest(row, forged, func(int) (string, error) {
		t.Fatal("caller binding must be rejected before reading a credential")
		return "", nil
	})
	require.ErrorIs(t, err, ErrInvalid)
}
