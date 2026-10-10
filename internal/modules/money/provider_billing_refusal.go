package money

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// A refusal records that a hold's provider cost will not qualify automatically:
// the qualifier refused its evidence, or the host cannot produce any. It is the
// one mark of a stuck hold. It fences observation, increment and release, and an
// operator's close is its only exit.

type ProviderBillingRefusal struct {
	Reason    ProviderBillingQualificationReason
	Detail    string
	RefusedAt time.Time
}

// recordHostRefusalInTx records, as an observation, that the host cannot
// qualify an open hold's provider cost: it carries a host refusal kind and no
// evidence. The refusal's detail names the observation, as the qualifier's
// does. Repeating the observation replays; a changed term conflicts.
func (s *MoneyService) recordHostRefusalInTx(ctx context.Context, txDB *db.DB, in ProviderBillingObservationInput) (*OperationAuthorization, error) {
	if err := validateHostRefusal(in); err != nil {
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
	reason := string(in.Refusal.Kind)
	detail := hostRefusalDetail(in)
	key := gen.GetProviderBillingRefusalParams{MerchantID: merchantID.UUID(), OperationID: in.OperationID}
	if existing, getErr := q.GetProviderBillingRefusal(ctx, key); getErr == nil {
		if !observationRefused(existing, in.ObservationID) {
			return nil, ErrProviderOperationRefused
		}
		switch {
		case existing.Reason != reason:
			return nil, &ProviderBillingObservationConflict{Field: "refusal_kind"}
		case providerBillingOptionalString(existing.Detail) != detail:
			return nil, &ProviderBillingObservationConflict{Field: "refusal_detail"}
		}
		auth := operationAuthorizationFromRow(authRow, true)
		return auth, attachOperationDetails(ctx, q, merchantID.UUID(), auth)
	} else if !errors.Is(getErr, pgx.ErrNoRows) {
		return nil, getErr
	}
	if OperationAuthorizationState(authRow.State) != OperationAuthorizationOpen {
		return nil, ErrProviderOperationNotOpen
	}
	// An observation id names one observation: evidence already recorded under
	// it is not this refusal.
	if _, err := q.GetProviderBillingObservation(ctx, gen.GetProviderBillingObservationParams{
		MerchantID: merchantID.UUID(), OperationID: in.OperationID, ObservationID: in.ObservationID,
	}); err == nil {
		return nil, &ProviderBillingObservationConflict{Field: "refusal_kind"}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if _, err := q.InsertProviderBillingRefusal(ctx, gen.InsertProviderBillingRefusalParams{
		MerchantID: merchantID.UUID(), OperationID: in.OperationID, Reason: reason,
		Detail: &detail, RefusedAt: s.now().UTC(),
	}); err != nil {
		return nil, err
	}
	auth := operationAuthorizationFromRow(authRow, false)
	return auth, attachOperationDetails(ctx, q, merchantID.UUID(), auth)
}

// validateHostRefusal requires a host refusal to carry no evidence.
func validateHostRefusal(in ProviderBillingObservationInput) error {
	if err := validateOperationID(in.OperationID); err != nil {
		return err
	}
	if err := validateOperationAuthorizationText("observation_id", in.ObservationID, operationAuthorizationMaxIDBytes); err != nil {
		return err
	}
	l := in.Lifecycle
	if l.Provider != "" || l.ProviderResourceID != "" || !l.ProviderLifetimeStartsAt.IsZero() || !l.ProviderLifetimeEndsAt.IsZero() ||
		!l.ProviderAbsentAt.IsZero() || l.ProviderAbsenceReference != "" || l.BillingStopReference != "" ||
		!l.WindowsClosedAt.IsZero() || l.WindowsClosedReference != "" || len(l.LifecycleEvidenceBody) != 0 {
		return fmt.Errorf("refusal %q carries no lifecycle", in.Refusal.Kind)
	}
	if in.NormalizedQuery != "" || !in.QueryStartsAt.IsZero() || !in.QueryEndsAt.IsZero() || len(in.RawBody) != 0 || len(in.Records) != 0 {
		return fmt.Errorf("refusal %q carries no query, raw body or records", in.Refusal.Kind)
	}
	if in.Refusal.Detail != "" {
		if err := validateOperationAuthorizationText("refusal.detail", in.Refusal.Detail, providerBillingMaxNoteBytes); err != nil {
			return err
		}
	}
	if len(hostRefusalDetail(in)) > providerBillingMaxNoteBytes {
		return fmt.Errorf("refusal.detail with its observation id exceeds %d bytes", providerBillingMaxNoteBytes)
	}
	return nil
}

// hostRefusalDetail is the refusal's detail: the observation that refused the
// hold, then the host's note.
func hostRefusalDetail(in ProviderBillingObservationInput) string {
	detail := "observation " + in.ObservationID
	if in.Refusal.Detail != "" {
		detail += ": " + in.Refusal.Detail
	}
	return detail
}

// observationRefused reports whether the refusal names this observation.
func observationRefused(row gen.BillingCostRefusal, observationID string) bool {
	detail := providerBillingOptionalString(row.Detail)
	prefix := "observation " + observationID
	return detail == prefix || strings.HasPrefix(detail, prefix+": ")
}

// refuseProviderBillingQualification records the qualifier's own refusal, in
// the commit that refused the qualification it stands on.
func refuseProviderBillingQualification(ctx context.Context, q *gen.Queries, qual gen.BillingCostQualification, observationID string, refusalKind *string, at time.Time) error {
	detail := "observation " + observationID
	if refusalKind != nil {
		detail += ": " + *refusalKind
	}
	_, err := q.InsertProviderBillingRefusal(ctx, gen.InsertProviderBillingRefusalParams{
		MerchantID: qual.MerchantID, OperationID: qual.OperationID, Reason: qual.Reason,
		Detail: &detail, RefusedAt: at,
	})
	return err
}

func providerBillingRefused(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, operationID string) (bool, error) {
	_, err := q.GetProviderBillingRefusal(ctx, gen.GetProviderBillingRefusalParams{MerchantID: merchantID, OperationID: operationID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// attachOperationDetails loads each operation's latest increment, its
// qualification, its refusal and the resolution that closed it.
func attachOperationDetails(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, auths ...*OperationAuthorization) error {
	if len(auths) == 0 {
		return nil
	}
	ids := make([]string, len(auths))
	byID := make(map[string]*OperationAuthorization, len(auths))
	for i, auth := range auths {
		ids[i] = auth.OperationID
		byID[auth.OperationID] = auth
	}
	increments, err := q.ListLastOperationAuthorizationExtensions(ctx, gen.ListLastOperationAuthorizationExtensionsParams{MerchantID: merchantID, OperationIds: ids})
	if err != nil {
		return err
	}
	for _, row := range increments {
		byID[row.OperationID].LastIncrement = operationAuthorizationExtensionFromRow(row)
	}
	qualifications, err := q.ListProviderBillingQualificationsForOperations(ctx, gen.ListProviderBillingQualificationsForOperationsParams{MerchantID: merchantID, OperationIds: ids})
	if err != nil {
		return err
	}
	for _, row := range qualifications {
		byID[row.OperationID].Qualification = providerBillingQualificationFromRow(row)
	}
	rows, err := q.ListProviderBillingRefusals(ctx, gen.ListProviderBillingRefusalsParams{MerchantID: merchantID, OperationIds: ids})
	if err != nil {
		return err
	}
	for _, row := range rows {
		auth := byID[row.OperationID]
		auth.Refusal = &ProviderBillingRefusal{
			Reason: ProviderBillingQualificationReason(row.Reason), Detail: providerBillingOptionalString(row.Detail), RefusedAt: row.RefusedAt,
		}
		if row.ResolutionKind != nil {
			auth.Resolution = &ProviderBillingResolution{
				Kind: ProviderBillingResolutionKind(*row.ResolutionKind), CostAmount: row.ResolutionCostAmount,
				AttestedBy: providerBillingOptionalString(row.ResolutionAttestedBy), Reference: providerBillingOptionalString(row.ResolutionReference),
				Note: providerBillingOptionalString(row.ResolutionNote), ResolvedAt: *row.ResolvedAt,
			}
		}
	}
	return nil
}

// ListOperationAuthorizations pages the merchant's operations, newest first,
// each with its details. Refused true with state open lists the holds
// waiting for an operator.
func (s *MoneyService) ListOperationAuthorizations(ctx context.Context, filter billing.ProviderOperationListParams) (billing.ListPage[*OperationAuthorization], error) {
	var page billing.ListPage[*OperationAuthorization]
	limit, err := pagination.Limit(filter.PageRequest)
	if err != nil {
		return page, err
	}
	var after operationAuthorizationCursor
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
	states := []string{}
	for _, state := range filter.State {
		switch state {
		case billing.ProviderOperationOpen, billing.ProviderOperationReleased, billing.ProviderOperationSettled:
		default:
			return page, apperr.New(http.StatusBadRequest, billing.CodeInvalidQuery, `state must be "open", "released" or "settled"`).WithParam("state")
		}
		states = append(states, string(state))
	}
	var afterAt *time.Time
	var afterID *string
	if present {
		afterAt, afterID = &after.At, &after.OperationID
	}
	var rows []gen.BillingOperationAuthorization
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := s.db.Gen(ctx)
		if filter.Refused != nil && *filter.Refused {
			refused, err := q.ListRefusedOperationAuthorizations(ctx, gen.ListRefusedOperationAuthorizationsParams{
				MerchantID: merchantID.UUID(), States: states, AfterAt: afterAt, AfterOperationID: afterID, RowLimit: pagination.Fetch(limit),
			})
			if err != nil {
				return err
			}
			for _, row := range refused {
				rows = append(rows, row.BillingOperationAuthorization)
			}
		} else {
			var err error
			rows, err = q.ListOperationAuthorizations(ctx, gen.ListOperationAuthorizationsParams{
				MerchantID: merchantID.UUID(), States: states, Refused: filter.Refused, AfterAt: afterAt, AfterOperationID: afterID, RowLimit: pagination.Fetch(limit),
			})
			if err != nil {
				return err
			}
		}
		page = pagination.Map(pagination.Cut(rows, limit, func(row gen.BillingOperationAuthorization) any {
			return operationAuthorizationCursor{At: row.CreatedAt, OperationID: row.OperationID}
		}), func(row gen.BillingOperationAuthorization) *OperationAuthorization {
			return operationAuthorizationFromRow(row, false)
		})
		return attachOperationDetails(ctx, q, merchantID.UUID(), page.Items...)
	})
	return page, err
}

type operationAuthorizationCursor struct {
	At          time.Time `json:"t"`
	OperationID string    `json:"o"`
}
