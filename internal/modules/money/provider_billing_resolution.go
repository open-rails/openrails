package money

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

const providerBillingMaxNoteBytes = 4 << 10

var ErrProviderOperationNotRefused = billing.ErrProviderOperationNotRefused

type ProviderBillingResolutionKind = billing.ProviderBillingResolutionKind

type ProviderBillingResolutionInput = billing.CloseProviderOperationParams

type ProviderBillingResolution struct {
	Kind       ProviderBillingResolutionKind
	CostAmount *int64
	AttestedBy string
	Reference  string
	Note       string
	ResolvedAt time.Time
}

// CloseOperationAuthorizationInTx closes a refused hold on an operator's
// attestation, in a caller-owned transaction: settled posts the attested cost
// through the pass-through settlement (above the hold as owed), written_off
// releases the hold uncharged. The attestation is recorded once; repeating it
// replays and any changed term conflicts.
func (s *MoneyService) CloseOperationAuthorizationInTx(ctx context.Context, txDB *db.DB, in ProviderBillingResolutionInput) (*OperationAuthorization, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if txDB == nil {
		return nil, fmt.Errorf("operation authorization close requires a bound transaction")
	}
	if err := validateProviderBillingResolution(in); err != nil {
		return nil, fmt.Errorf("%w: %v", billing.ErrInvalid, err)
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	txSvc := &MoneyService{db: txDB, clock: s.clock}
	q := txDB.Gen(ctx)
	params := gen.GetOperationAuthorizationParams{MerchantID: merchantID.UUID(), OperationID: in.OperationID}
	authRow, err := q.GetOperationAuthorization(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProviderOperationNotFound
	}
	if err != nil {
		return nil, err
	}
	// The payer row is the money mutex shared with observation and settlement.
	if _, err := txSvc.lockBalance(ctx, q, identity.CustomerID(authRow.CustomerID), authRow.RecordOwner, operationAuthorizationCurrency); err != nil {
		return nil, err
	}
	if authRow, err = q.GetOperationAuthorization(ctx, params); err != nil {
		return nil, err
	}
	resolutionKey := gen.GetProviderBillingResolutionParams{MerchantID: merchantID.UUID(), OperationID: in.OperationID}
	if existing, getErr := q.GetProviderBillingResolution(ctx, resolutionKey); getErr == nil {
		if err := replayProviderBillingResolution(existing, in); err != nil {
			return nil, err
		}
		auth := operationAuthorizationFromRow(authRow, true)
		return auth, attachOperationDetails(ctx, q, merchantID.UUID(), auth)
	} else if !errors.Is(getErr, pgx.ErrNoRows) {
		return nil, getErr
	}
	refusal, err := q.GetProviderBillingRefusal(ctx, gen.GetProviderBillingRefusalParams{MerchantID: merchantID.UUID(), OperationID: in.OperationID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProviderOperationNotRefused
	}
	if err != nil {
		return nil, err
	}
	if OperationAuthorizationState(authRow.State) != OperationAuthorizationOpen {
		return nil, ErrProviderOperationNotOpen
	}

	now := s.now().UTC()
	resolution, err := q.InsertProviderBillingResolution(ctx, gen.InsertProviderBillingResolutionParams{
		MerchantID:  merchantID.UUID(),
		OperationID: in.OperationID,
		Kind:        string(in.Kind),
		CostAmount:  in.CostAmount,
		AttestedBy:  in.AttestedBy,
		Reference:   in.Reference,
		Note:        optionalText(in.Note),
		ResolvedAt:  now,
	})
	if err != nil {
		return nil, err
	}
	var auth *OperationAuthorization
	switch in.Kind {
	case billing.ProviderBillingResolutionSettled:
		body, err := providerBillingResolutionBody(ctx, q, refusal, authRow, resolution)
		if err != nil {
			return nil, err
		}
		auth, err = txSvc.settlePassThroughProviderCostInTx(ctx, txDB, passThroughProviderCostSettlementInput{
			OperationID: in.OperationID, CostAmount: *in.CostAmount, SettlementBody: body,
		})
		if err != nil {
			return nil, err
		}
	case billing.ProviderBillingResolutionWrittenOff:
		released, err := q.WriteOffOperationAuthorization(ctx, gen.WriteOffOperationAuthorizationParams{
			TerminalReference: in.Reference, ReleasedAt: now,
			MerchantID: merchantID.UUID(), OperationID: in.OperationID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("operation authorization changed state under the payer lock")
		}
		if err != nil {
			return nil, err
		}
		auth = operationAuthorizationFromRow(released, false)
	}
	return auth, attachOperationDetails(ctx, q, merchantID.UUID(), auth)
}

func validateProviderBillingResolution(in ProviderBillingResolutionInput) error {
	if err := validateOperationID(in.OperationID); err != nil {
		return err
	}
	switch in.Kind {
	case billing.ProviderBillingResolutionSettled:
		if in.CostAmount == nil || *in.CostAmount < 0 {
			return fmt.Errorf("a settled resolution requires a nonnegative cost_amount")
		}
	case billing.ProviderBillingResolutionWrittenOff:
		if in.CostAmount != nil {
			return fmt.Errorf("a written_off resolution takes no cost_amount")
		}
	default:
		return fmt.Errorf("kind must be %q or %q", billing.ProviderBillingResolutionSettled, billing.ProviderBillingResolutionWrittenOff)
	}
	if err := validateOperationAuthorizationText("attested_by", in.AttestedBy, operationAuthorizationMaxPrincipalBytes); err != nil {
		return err
	}
	if err := validateOperationAuthorizationText("reference", in.Reference, operationAuthorizationMaxReferenceBytes); err != nil {
		return err
	}
	if in.Note != "" {
		if err := validateOperationAuthorizationText("note", in.Note, providerBillingMaxNoteBytes); err != nil {
			return err
		}
	}
	return nil
}

func replayProviderBillingResolution(row gen.BillingCostResolution, in ProviderBillingResolutionInput) error {
	checks := []struct {
		field string
		same  bool
	}{
		{"kind", row.Kind == string(in.Kind)},
		{"cost_amount", equalOptionalInt64(row.CostAmount, in.CostAmount)},
		{"attested_by", row.AttestedBy == in.AttestedBy},
		{"reference", row.Reference == in.Reference},
		{"note", providerBillingOptionalString(row.Note) == in.Note},
	}
	for _, check := range checks {
		if !check.same {
			return &billing.ProviderOperationConflict{Field: check.field}
		}
	}
	return nil
}

func optionalText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// providerBillingResolutionManifest is the settlement body of an
// operator-attested settlement: the hold, why it was refused, any provider
// evidence it carried, and the attestation that replaced qualification. The
// qualification facts are null for a hold the host refused before any evidence.
type providerBillingResolutionManifest struct {
	Contract                 string                                 `json:"contract"`
	OperationID              string                                 `json:"operation_id"`
	Provider                 *string                                `json:"provider"`
	ProviderResourceID       *string                                `json:"provider_resource_id"`
	ProviderLifetimeStartsAt *string                                `json:"provider_lifetime_starts_at"`
	ProviderLifetimeEndsAt   *string                                `json:"provider_lifetime_ends_at"`
	ProviderAbsentAt         *string                                `json:"provider_absent_at"`
	LifecycleEvidenceSHA256  *string                                `json:"lifecycle_evidence_sha256"`
	Authorization            providerBillingSettlementAuthorization `json:"authorization"`
	Refusal                  providerBillingManifestRefusal         `json:"refusal"`
	QualificationReason      *string                                `json:"qualification_reason"`
	BaselineObservationID    *string                                `json:"baseline_observation_id"`
	RefusedObservation       *providerBillingRefusedObservation     `json:"refused_observation"`
	CostAmount               int64                                  `json:"cost_amount,string"`
	AttestedBy               string                                 `json:"attested_by"`
	Reference                string                                 `json:"reference"`
	Note                     *string                                `json:"note"`
	ResolvedAt               string                                 `json:"resolved_at"`
}

type providerBillingManifestRefusal struct {
	Reason    string  `json:"reason"`
	Detail    *string `json:"detail"`
	RefusedAt string  `json:"refused_at"`
}

type providerBillingRefusedObservation struct {
	providerBillingSettlementObservation
	RefusalKind *string `json:"refusal_kind"`
	CostAmount  *int64  `json:"cost_amount,string"`
}

func providerBillingResolutionBody(ctx context.Context, q *gen.Queries, refusal gen.BillingCostRefusal, auth gen.BillingOperationAuthorization, resolution gen.BillingCostResolution) ([]byte, error) {
	authorization, err := providerBillingAuthorizationManifest(ctx, q, auth)
	if err != nil {
		return nil, err
	}
	manifest := providerBillingResolutionManifest{
		Contract:      "openrails/operator-attested-provider-cost",
		OperationID:   auth.OperationID,
		Authorization: authorization,
		Refusal: providerBillingManifestRefusal{
			Reason: refusal.Reason, Detail: refusal.Detail, RefusedAt: refusal.RefusedAt.UTC().Format(time.RFC3339Nano),
		},
		CostAmount: *resolution.CostAmount,
		AttestedBy: resolution.AttestedBy,
		Reference:  resolution.Reference,
		Note:       resolution.Note,
		ResolvedAt: resolution.ResolvedAt.UTC().Format(time.RFC3339Nano),
	}
	row, err := q.GetProviderBillingQualificationForUpdate(ctx, gen.GetProviderBillingQualificationForUpdateParams{
		MerchantID: auth.MerchantID, OperationID: auth.OperationID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		stamp := func(t time.Time) *string { v := t.UTC().Format(time.RFC3339Nano); return &v }
		lifecycle := hex.EncodeToString(row.LifecycleEvidenceDigest)
		manifest.Provider, manifest.ProviderResourceID = &row.Provider, &row.ProviderResourceID
		manifest.ProviderLifetimeStartsAt, manifest.ProviderLifetimeEndsAt = stamp(row.ProviderLifetimeStartsAt), stamp(row.ProviderLifetimeEndsAt)
		manifest.ProviderAbsentAt, manifest.LifecycleEvidenceSHA256 = stamp(row.ProviderAbsentAt), &lifecycle
		manifest.QualificationReason, manifest.BaselineObservationID = &row.Reason, row.BaselineObservationID
		if ProviderBillingQualificationState(row.State) == ProviderBillingQualificationRefused {
			refused, err := q.GetLatestProviderBillingObservation(ctx, gen.GetLatestProviderBillingObservationParams{
				MerchantID: row.MerchantID, OperationID: row.OperationID,
			})
			if err != nil {
				return nil, fmt.Errorf("load refusing provider billing observation: %w", err)
			}
			if refused.QualificationReason != row.Reason {
				return nil, fmt.Errorf("latest provider billing observation %q did not refuse the qualification", refused.ObservationID)
			}
			manifest.RefusedObservation = &providerBillingRefusedObservation{
				providerBillingSettlementObservation: providerBillingSettlementObservationFromRow(gen.GetProviderBillingObservationRow(refused)),
				RefusalKind:                          refused.RefusalKind,
				CostAmount:                           refused.CostAmount,
			}
		}
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("author provider billing resolution body: %w", err)
	}
	return body, nil
}
