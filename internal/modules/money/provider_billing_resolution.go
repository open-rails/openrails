package money

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

const providerBillingMaxNoteBytes = 4 << 10

var (
	ErrProviderBillingQualificationNotRefused = billing.ErrProviderBillingQualificationNotRefused
	ErrProviderBillingResolutionConflict      = billing.ErrProviderBillingResolutionConflict
)

type ProviderBillingResolutionKind = billing.ProviderBillingResolutionKind

type ProviderBillingResolutionInput = billing.ResolveProviderBillingQualificationParams

type ProviderBillingResolution struct {
	Kind       ProviderBillingResolutionKind
	CostAmount *int64
	AttestedBy string
	Reference  string
	Note       string
	ResolvedAt time.Time
}

// ResolveProviderBillingQualificationInTx closes the open authorization of a
// refused qualification on an operator's attestation, in a caller-owned
// transaction: settled posts the attested cost through the pass-through
// settlement, written_off releases the hold. The attestation is recorded once;
// repeating it replays and any changed term conflicts.
func (s *MoneyService) ResolveProviderBillingQualificationInTx(ctx context.Context, txDB *db.DB, in ProviderBillingResolutionInput) (*ProviderBillingQualification, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if txDB == nil {
		return nil, fmt.Errorf("provider billing resolution requires a bound transaction")
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
		return nil, ErrOperationAuthorizationNotFound
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
	qual, err := q.GetProviderBillingQualificationForUpdate(ctx, gen.GetProviderBillingQualificationForUpdateParams{
		MerchantID: merchantID.UUID(), OperationID: in.OperationID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProviderBillingQualificationNotFound
	}
	if err != nil {
		return nil, err
	}
	resolutionKey := gen.GetProviderBillingResolutionParams{MerchantID: merchantID.UUID(), OperationID: in.OperationID}
	if existing, getErr := q.GetProviderBillingResolution(ctx, resolutionKey); getErr == nil {
		if err := replayProviderBillingResolution(existing, in); err != nil {
			return nil, err
		}
		result := providerBillingQualificationFromRow(qual, operationAuthorizationFromRow(authRow, false), true)
		result.Resolution = providerBillingResolutionFromRow(existing)
		return result, nil
	} else if !errors.Is(getErr, pgx.ErrNoRows) {
		return nil, getErr
	}
	if ProviderBillingQualificationState(qual.State) != ProviderBillingQualificationRefused {
		return nil, ErrProviderBillingQualificationNotRefused
	}
	if OperationAuthorizationState(authRow.State) != OperationAuthorizationOpen {
		return nil, ErrOperationAuthorizationNotOpen
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
		body, err := providerBillingResolutionBody(ctx, q, qual, authRow, resolution)
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
	result := providerBillingQualificationFromRow(qual, auth, false)
	result.Resolution = providerBillingResolutionFromRow(resolution)
	return result, nil
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
			return &billing.ProviderBillingResolutionConflict{Field: check.field}
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

func providerBillingResolutionFromRow(row gen.BillingCostResolution) *ProviderBillingResolution {
	return &ProviderBillingResolution{
		Kind:       ProviderBillingResolutionKind(row.Kind),
		CostAmount: row.CostAmount,
		AttestedBy: row.AttestedBy,
		Reference:  row.Reference,
		Note:       providerBillingOptionalString(row.Note),
		ResolvedAt: row.ResolvedAt,
	}
}

// providerBillingResolutionManifest is the settlement body of an
// operator-attested settlement: the refused evidence, the hold, and the
// attestation that replaced qualification.
type providerBillingResolutionManifest struct {
	Contract                 string                                 `json:"contract"`
	OperationID              string                                 `json:"operation_id"`
	Provider                 string                                 `json:"provider"`
	ProviderResourceID       string                                 `json:"provider_resource_id"`
	ProviderLifetimeStartsAt string                                 `json:"provider_lifetime_starts_at"`
	ProviderLifetimeEndsAt   string                                 `json:"provider_lifetime_ends_at"`
	ProviderAbsentAt         string                                 `json:"provider_absent_at"`
	LifecycleEvidenceSHA256  string                                 `json:"lifecycle_evidence_sha256"`
	Authorization            providerBillingSettlementAuthorization `json:"authorization"`
	QualificationReason      string                                 `json:"qualification_reason"`
	BaselineObservationID    *string                                `json:"baseline_observation_id"`
	RefusedObservation       providerBillingRefusedObservation      `json:"refused_observation"`
	CostAmount               int64                                  `json:"cost_amount,string"`
	AttestedBy               string                                 `json:"attested_by"`
	Reference                string                                 `json:"reference"`
	Note                     *string                                `json:"note"`
	ResolvedAt               string                                 `json:"resolved_at"`
}

type providerBillingRefusedObservation struct {
	providerBillingSettlementObservation
	RefusalKind *string `json:"refusal_kind"`
	CostAmount  *int64  `json:"cost_amount,string"`
}

func providerBillingResolutionBody(ctx context.Context, q *gen.Queries, row gen.BillingCostQualification, auth gen.BillingOperationAuthorization, resolution gen.BillingCostResolution) ([]byte, error) {
	refused, err := q.GetLatestProviderBillingObservation(ctx, gen.GetLatestProviderBillingObservationParams{
		MerchantID: row.MerchantID, OperationID: row.OperationID,
	})
	if err != nil {
		return nil, fmt.Errorf("load refusing provider billing observation: %w", err)
	}
	if refused.QualificationReason != row.Reason {
		return nil, fmt.Errorf("latest provider billing observation %q did not refuse the qualification", refused.ObservationID)
	}
	authorization, err := providerBillingAuthorizationManifest(ctx, q, auth)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(providerBillingResolutionManifest{
		Contract:                 "openrails/operator-attested-provider-cost",
		OperationID:              row.OperationID,
		Provider:                 row.Provider,
		ProviderResourceID:       row.ProviderResourceID,
		ProviderLifetimeStartsAt: row.ProviderLifetimeStartsAt.UTC().Format(time.RFC3339Nano),
		ProviderLifetimeEndsAt:   row.ProviderLifetimeEndsAt.UTC().Format(time.RFC3339Nano),
		ProviderAbsentAt:         row.ProviderAbsentAt.UTC().Format(time.RFC3339Nano),
		LifecycleEvidenceSHA256:  hex.EncodeToString(row.LifecycleEvidenceDigest),
		Authorization:            authorization,
		QualificationReason:      row.Reason,
		BaselineObservationID:    row.BaselineObservationID,
		RefusedObservation: providerBillingRefusedObservation{
			providerBillingSettlementObservation: providerBillingSettlementObservationFromRow(gen.GetProviderBillingObservationRow(refused)),
			RefusalKind:                          refused.RefusalKind,
			CostAmount:                           refused.CostAmount,
		},
		CostAmount: *resolution.CostAmount,
		AttestedBy: resolution.AttestedBy,
		Reference:  resolution.Reference,
		Note:       resolution.Note,
		ResolvedAt: resolution.ResolvedAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, fmt.Errorf("author provider billing resolution body: %w", err)
	}
	return body, nil
}

// ListProviderBillingQualifications pages the merchant's qualifications, newest
// first, each with its authorization and any operator resolution.
func (s *MoneyService) ListProviderBillingQualifications(ctx context.Context, filter billing.ProviderBillingQualificationListParams) (billing.ListPage[*ProviderBillingQualification], error) {
	var page billing.ListPage[*ProviderBillingQualification]
	limit, err := pagination.Limit(filter.PageRequest)
	if err != nil {
		return page, err
	}
	var after providerBillingQualificationCursor
	present, err := pagination.Decode(filter.Cursor, &after)
	if err != nil {
		return page, err
	}
	if present && (after.At.IsZero() || after.OperationID == "") {
		return page, pagination.ErrInvalidCursor
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return page, err
	}
	params := gen.ListProviderBillingQualificationsParams{
		MerchantID: merchantID.UUID(), States: []string{}, AuthorizationStates: []string{}, RowLimit: pagination.Fetch(limit),
	}
	for _, state := range filter.State {
		switch state {
		case billing.ProviderBillingQualificationPending, billing.ProviderBillingQualificationRefused, billing.ProviderBillingQualificationEligible:
		default:
			return page, apperr.New(http.StatusBadRequest, billing.CodeInvalidQuery, `state must be "pending", "refused" or "eligible"`).WithParam("state")
		}
		params.States = append(params.States, string(state))
	}
	for _, state := range filter.AuthorizationState {
		switch state {
		case billing.OperationAuthorizationOpen, billing.OperationAuthorizationReleased, billing.OperationAuthorizationSettled:
		default:
			return page, apperr.New(http.StatusBadRequest, billing.CodeInvalidQuery, `authorization_state must be "open", "released" or "settled"`).WithParam("authorization_state")
		}
		params.AuthorizationStates = append(params.AuthorizationStates, string(state))
	}
	if present {
		params.AfterAt, params.AfterOperationID = &after.At, &after.OperationID
	}
	var rows []gen.ListProviderBillingQualificationsRow
	var resolutions []gen.BillingCostResolution
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := s.db.Gen(ctx)
		var err error
		if rows, err = q.ListProviderBillingQualifications(ctx, params); err != nil || len(rows) == 0 {
			return err
		}
		ids := make([]string, len(rows))
		for i, row := range rows {
			ids[i] = row.BillingCostQualification.OperationID
		}
		resolutions, err = q.ListProviderBillingResolutions(ctx, gen.ListProviderBillingResolutionsParams{MerchantID: merchantID.UUID(), OperationIds: ids})
		return err
	})
	if err != nil {
		return page, err
	}
	resolved := make(map[string]gen.BillingCostResolution, len(resolutions))
	for _, r := range resolutions {
		resolved[r.OperationID] = r
	}
	cut := pagination.Cut(rows, limit, func(row gen.ListProviderBillingQualificationsRow) any {
		return providerBillingQualificationCursor{At: row.BillingCostQualification.CreatedAt, OperationID: row.BillingCostQualification.OperationID}
	})
	return pagination.Map(cut, func(row gen.ListProviderBillingQualificationsRow) *ProviderBillingQualification {
		out := providerBillingQualificationFromRow(row.BillingCostQualification, operationAuthorizationFromRow(row.BillingOperationAuthorization, false), false)
		if r, ok := resolved[out.OperationID]; ok {
			out.Resolution = providerBillingResolutionFromRow(r)
		}
		return out
	}), nil
}

type providerBillingQualificationCursor struct {
	At          time.Time `json:"t"`
	OperationID string    `json:"o"`
}

// withProviderBillingResolution attaches the operation's resolution, if any.
func withProviderBillingResolution(ctx context.Context, q *gen.Queries, out *ProviderBillingQualification) (*ProviderBillingQualification, error) {
	row, err := q.GetProviderBillingResolution(ctx, gen.GetProviderBillingResolutionParams{MerchantID: out.MerchantID, OperationID: out.OperationID})
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	out.Resolution = providerBillingResolutionFromRow(row)
	return out, nil
}
