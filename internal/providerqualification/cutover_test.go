package providerqualification

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/stretchr/testify/require"
)

func manifest(t *testing.T, record any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"settings": map[string]any{setting: record}})
	require.NoError(t, err)
	return raw
}

func setVersion(t *testing.T, evidence []byte, key string, v int) []byte {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(evidence, &doc))
	doc["credential_versions"].(map[string]any)[key] = v
	out, err := json.Marshal(doc)
	require.NoError(t, err)
	return out
}

// The server, not the caller, binds the security-key credential and its version;
// a later security-key rotation unqualifies, an unrelated secret does not.
func TestQualificationBindsOnlyServerCredentialAndItsVersion(t *testing.T) {
	id := uuid.New()
	record := Record{PSPID: id, Environment: "test", Contract: NMIContract, EvidenceRef: "operator-proof"}
	input := manifest(t, record)
	row := gen.OpenrailsPsp{ID: id, Rail: "nmi", Environment: "test", Evidence: []byte(`{"credential_versions":{"security_key":7,"webhook_signing_secret":2}}`)}
	bound, err := BindManifest(row, input, func(version int) (string, error) {
		require.Equal(t, 7, version)
		return " current credential ", nil
	})
	require.NoError(t, err)
	row.Evidence = bound
	got, err := Current(row)
	require.NoError(t, err)
	require.Equal(t, Fingerprint("current credential"), got.CredentialFingerprint)
	require.Equal(t, 7, *got.CredentialVersion)
	require.Equal(t, record, got.Record)

	row.Evidence = setVersion(t, bound, "webhook_signing_secret", 3)
	_, err = Current(row)
	require.NoError(t, err, "an unrelated webhook secret is not payment-account authority")
	row.Evidence = setVersion(t, bound, "security_key", 8)
	_, err = Current(row)
	require.ErrorIs(t, err, ErrUnqualified)

	row.Evidence = input
	_, err = Current(row)
	require.ErrorIs(t, err, ErrInvalid, "unbound pre-cut records are not grandfathered")

	forged := manifest(t, BoundRecord{Record: record, CredentialFingerprint: Fingerprint("forged")})
	_, err = BindManifest(row, forged, func(int) (string, error) {
		t.Fatal("caller binding must be rejected before reading a credential")
		return "", nil
	})
	require.ErrorIs(t, err, ErrInvalid)

	boom := errors.New("secret store down")
	_, err = BindManifest(row, input, func(int) (string, error) { return "", boom })
	require.ErrorIs(t, err, boom)
	_, err = BindManifest(row, input, func(int) (string, error) { return "  ", nil })
	require.ErrorIs(t, err, ErrInvalid, "an empty credential cannot be bound")
}

func TestQualificationRecordValidation(t *testing.T) {
	id := uuid.New()
	row := gen.OpenrailsPsp{ID: id, Rail: "nmi", Environment: "live"}
	good := Record{PSPID: id, Environment: "live", Contract: NMIContract, EvidenceRef: "ticket:OPS-1/run_2"}
	for name, mut := range map[string]func(*Record){
		"other psp":           func(r *Record) { r.PSPID = uuid.New() },
		"nil psp":             func(r *Record) { r.PSPID = uuid.Nil },
		"other environment":   func(r *Record) { r.Environment = "test" },
		"other contract":      func(r *Record) { r.Contract = "nmi-anything-v2" },
		"blank evidence":      func(r *Record) { r.EvidenceRef = "" },
		"evidence with PAN":   func(r *Record) { r.EvidenceRef = "card-4111111111111111" },
		"evidence too long":   func(r *Record) { r.EvidenceRef = "a" + strings.Repeat("b", 200) },
		"evidence with space": func(r *Record) { r.EvidenceRef = "operator proof" },
	} {
		rec := good
		mut(&rec)
		_, err := BindManifest(row, manifest(t, rec), func(int) (string, error) { return "k", nil })
		require.ErrorIs(t, err, ErrInvalid, name)
	}
	_, err := BindManifest(gen.OpenrailsPsp{ID: id, Rail: "stripe", Environment: "live"}, manifest(t, good), func(int) (string, error) { return "k", nil })
	require.ErrorIs(t, err, ErrInvalid, "only NMI accounts qualify")
	_, err = BindManifest(row, manifest(t, map[string]any{"psp_id": id, "environment": "live", "contract": NMIContract, "evidence_ref": "x", "extra": 1}), nil)
	require.ErrorIs(t, err, ErrInvalid, "unknown fields are refused")
	_, err = BindManifest(gen.OpenrailsPsp{ID: id, Rail: "nmi", Environment: "live", Evidence: []byte(`{"credential_versions":{"security_key":-1}}`)}, manifest(t, good), nil)
	require.ErrorIs(t, err, ErrInvalid)

	// Absence is "unqualified", passed through untouched; it is never an error.
	for _, input := range []string{`{}`, `{"settings":{}}`, `{"settings":{"nmi_cutover_qualification":null}}`} {
		out, err := BindManifest(row, []byte(input), nil)
		require.NoError(t, err)
		require.JSONEq(t, input, string(out))
		row.Evidence = []byte(input)
		rec, err := Current(row)
		require.NoError(t, err)
		require.Nil(t, rec)
	}
}

func TestFingerprintShape(t *testing.T) {
	require.Empty(t, Fingerprint(" "))
	require.True(t, ValidFingerprint(Fingerprint("k")))
	require.Equal(t, Fingerprint("k"), Fingerprint(" k\n"))
	require.False(t, ValidFingerprint(strings.ToUpper(Fingerprint("k"))))
	require.False(t, ValidFingerprint(Fingerprint("k")[:63]+"z"))
	require.False(t, ValidFingerprint(""))
}
