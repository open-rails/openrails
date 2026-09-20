package providerqualification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

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

func validate(record *Record, row gen.OpenrailsPsp) error {
	if record == nil {
		return nil
	}
	if row.Rail != "nmi" || record.PSPID == uuid.Nil || record.PSPID != row.ID ||
		record.Environment != row.Environment || record.Contract != NMIContract ||
		!evidenceReference.MatchString(record.EvidenceRef) || cardguard.ContainsPAN(record.EvidenceRef) {
		return ErrInvalid
	}
	return nil
}

// Current validates the same stored shape used by manifest ingestion and the
// control-plane setter. Absence means unqualified; malformed records refuse.
func Current(row gen.OpenrailsPsp) (*Record, error) {
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
	var record Record
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, ErrInvalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, ErrInvalid
	}
	if err := validate(&record, row); err != nil {
		return nil, err
	}
	return &record, nil
}

func Read(ctx context.Context, database *db.DB, pspID uuid.UUID) (*Record, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := database.Gen(ctx).GetPSP(ctx, pspID)
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
func Set(ctx context.Context, database *db.DB, pspID uuid.UUID, record *Record) error {
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
			raw, err = json.Marshal(record)
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
func WithWrite(ctx context.Context, database *db.DB, pspID uuid.UUID, send func() error) (entered bool, err error) {
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
		if record == nil {
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
