package providerqualification

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

const NMIContract = "nmi-paused-fixed-day-usd-v1"
const setting = "nmi_cutover_qualification"

var (
	ErrInvalid        = errors.New("invalid provider cutover qualification")
	ErrNotFound       = errors.New("provider account not found")
	ErrUnqualified    = errors.New("provider account is not qualified for cutover")
	evidenceReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._/-]{0,199}$`)
)

// Record is an operator's reference to completed external qualification, not
// a result inferred from a successful credential probe or a local test.
type Record struct {
	PSPID       uuid.UUID `json:"psp_id"`
	Environment string    `json:"environment"`
	Contract    string    `json:"contract"`
	EvidenceRef string    `json:"evidence_ref"`
}

// BoundRecord is private storage. Callers supply only Record; the server binds
// its selected credential before persisting qualification.
type BoundRecord struct {
	Record
	CredentialFingerprint string `json:"credential_fingerprint"`
	CredentialVersion     *int   `json:"credential_version"`
}

func Fingerprint(credential string) string {
	if strings.TrimSpace(credential) == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(credential))))
}

// CredentialVersion reads the existing cross-node security-key watermark.
func CredentialVersion(row gen.OpenrailsPsp) (int, error) {
	var document struct {
		Versions map[string]int `json:"credential_versions"`
	}
	if len(row.Evidence) > 0 {
		if err := json.Unmarshal(row.Evidence, &document); err != nil {
			return 0, ErrInvalid
		}
	}
	version := document.Versions["security_key"]
	if version < 0 {
		return 0, ErrInvalid
	}
	return version, nil
}

func ValidFingerprint(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func ValidEvidenceReference(value string) bool {
	return evidenceReference.MatchString(value) && !cardguard.ContainsPAN(value)
}

// BindManifest accepts the public qualification shape only. In particular, a
// supplied fingerprint is rejected; the secret reader owns that value.
func BindManifest(row gen.OpenrailsPsp, input []byte, credential func(int) (string, error)) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(input, &document); err != nil {
		return nil, ErrInvalid
	}
	var settings map[string]json.RawMessage
	if raw := document["settings"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &settings); err != nil {
			return nil, ErrInvalid
		}
	}
	raw := settings[setting]
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return input, nil
	}
	var record Record
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, ErrInvalid
	}
	if err := validate(&record, row); err != nil {
		return nil, err
	}
	version, err := CredentialVersion(row)
	if err != nil {
		return nil, err
	}
	key, err := credential(version)
	if err != nil {
		return nil, err
	}
	bound := BoundRecord{Record: record, CredentialFingerprint: Fingerprint(key), CredentialVersion: &version}
	if !ValidFingerprint(bound.CredentialFingerprint) {
		return nil, ErrInvalid
	}
	var previous map[string]json.RawMessage
	if len(row.Evidence) > 0 {
		if err := json.Unmarshal(row.Evidence, &previous); err != nil {
			return nil, ErrInvalid
		}
	}
	if raw := previous["credential_versions"]; len(raw) > 0 {
		document["credential_versions"] = raw
	} else {
		delete(document, "credential_versions")
	}
	settings[setting], err = json.Marshal(bound)
	if err != nil {
		return nil, err
	}
	document["settings"], err = json.Marshal(settings)
	if err != nil {
		return nil, err
	}
	return json.Marshal(document)
}

func validate(record *Record, row gen.OpenrailsPsp) error {
	if record == nil {
		return nil
	}
	if row.Rail != "nmi" || record.PSPID == uuid.Nil || record.PSPID != row.ID ||
		record.Environment != row.Environment || record.Contract != NMIContract ||
		!ValidEvidenceReference(record.EvidenceRef) {
		return ErrInvalid
	}
	return nil
}

// Current validates the same stored shape used by manifest ingestion and the
// control-plane setter. Absence means unqualified; malformed records refuse.
func Current(row gen.OpenrailsPsp) (*BoundRecord, error) {
	var doc struct {
		Settings map[string]json.RawMessage `json:"settings"`
	}
	if len(row.Evidence) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(row.Evidence, &doc); err != nil {
		return nil, ErrInvalid
	}
	raw := doc.Settings[setting]
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var record BoundRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, ErrInvalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, ErrInvalid
	}
	if err := validate(&record.Record, row); err != nil {
		return nil, err
	}
	if !ValidFingerprint(record.CredentialFingerprint) || record.CredentialVersion == nil {
		return nil, ErrInvalid
	}
	version, err := CredentialVersion(row)
	if err != nil {
		return nil, err
	}
	if *record.CredentialVersion != version {
		return nil, ErrUnqualified
	}
	return &record, nil
}

func Read(ctx context.Context, database *db.DB, pspID uuid.UUID) (*BoundRecord, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := database.Gen(ctx).GetPSP(ctx, gen.GetPSPParams{ID: pspID, MerchantID: mid.UUID()})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.MerchantID != mid.UUID()) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	record, err := Current(row)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, ErrUnqualified
	}
	return record, nil
}

// Set changes only this private settings record. The row update serializes with
// WithWrite's share lock; nil revokes without touching credentials or history.
func Set(ctx context.Context, database *db.DB, pspID uuid.UUID, record *Record, fingerprint string, observedVersion int) error {
	if pspID == uuid.Nil {
		return ErrInvalid
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := database.NewWithPgxTx(tx).Gen(ctx)
		row, err := q.GetPSPForQualificationUpdate(ctx, gen.GetPSPForQualificationUpdateParams{ID: pspID, MerchantID: mid.UUID()})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err = validate(record, row); err != nil {
			return err
		}
		var raw []byte
		if record != nil {
			version, err := CredentialVersion(row)
			if err != nil {
				return err
			}
			if !ValidFingerprint(fingerprint) || version != observedVersion {
				return ErrInvalid
			}
			raw, err = json.Marshal(BoundRecord{Record: *record, CredentialFingerprint: fingerprint, CredentialVersion: &version})
			if err != nil {
				return err
			}
		}
		_, err = q.SetPSPCutoverQualification(ctx, gen.SetPSPCutoverQualificationParams{ID: pspID, MerchantID: mid.UUID(), Qualification: raw})
		return err
	})
}

// WithWrite holds the written account's qualification stable until HTTP has
// returned. It reuses the current merchant pin, so a one-connection host pool
// works. Submission markers must commit before entering; results persist after.
// entered is false only when this invocation never called the HTTP callback.
func WithWrite(ctx context.Context, database *db.DB, pspID uuid.UUID, fingerprint string, send func() error) (entered bool, err error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		row, err := database.NewWithPgxTx(tx).Gen(ctx).GetPSPForCutoverWrite(ctx, gen.GetPSPForCutoverWriteParams{ID: pspID, MerchantID: mid.UUID()})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		record, err := Current(row)
		if err != nil {
			return err
		}
		if record == nil || !ValidFingerprint(fingerprint) || record.CredentialFingerprint != fingerprint {
			return ErrUnqualified
		}
		entered = true
		return send()
	})
	if err != nil {
		err = fmt.Errorf("provider cutover write qualification: %w", err)
	}
	return entered, err
}
