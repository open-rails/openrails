package money

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
// one mark of a stuck hold. It fences observation, extension and release, and an
// operator's close is its only exit.

const (
	ProviderBillingLifecycleUnprovable ProviderBillingQualificationReason = "lifecycle_unprovable"
	ProviderBillingUnavailable         ProviderBillingQualificationReason = "provider_billing_unavailable"
	ProviderBillingObservationRejected ProviderBillingQualificationReason = "observation_rejected"
)

var ErrProviderBillingRefusalConflict = billing.ErrProviderBillingRefusalConflict

type ProviderBillingRefusalInput = billing.RefuseProviderBillingQualificationParams

type ProviderBillingRefusal struct {
	Reason    ProviderBillingQualificationReason
	Detail    string
	RefusedAt time.Time
}

// RefuseProviderBillingQualificationInTx records, in a caller-owned
// transaction, that the host cannot qualify an open hold's provider cost. The
// hold then waits for an operator's close. Repeating the same refusal replays;
// a changed term conflicts.
func (s *MoneyService) RefuseProviderBillingQualificationInTx(ctx context.Context, txDB *db.DB, in ProviderBillingRefusalInput) (*OperationAuthorization, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if txDB == nil {
		return nil, fmt.Errorf("provider billing refusal requires a bound transaction")
	}
	if err := validateProviderBillingRefusal(in); err != nil {
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
	key := gen.GetProviderBillingRefusalParams{MerchantID: merchantID.UUID(), OperationID: in.OperationID}
	if existing, getErr := q.GetProviderBillingRefusal(ctx, key); getErr == nil {
		if err := replayProviderBillingRefusal(existing, in); err != nil {
			return nil, err
		}
		auth := operationAuthorizationFromRow(authRow, true)
		return auth, attachHoldOutcomes(ctx, q, merchantID.UUID(), auth)
	} else if !errors.Is(getErr, pgx.ErrNoRows) {
		return nil, getErr
	}
	if OperationAuthorizationState(authRow.State) != OperationAuthorizationOpen {
		return nil, ErrOperationAuthorizationNotOpen
	}
	if _, err := q.InsertProviderBillingRefusal(ctx, gen.InsertProviderBillingRefusalParams{
		MerchantID: merchantID.UUID(), OperationID: in.OperationID, Reason: string(in.Reason),
		Detail: optionalText(in.Detail), RefusedAt: s.now().UTC(),
	}); err != nil {
		return nil, err
	}
	auth := operationAuthorizationFromRow(authRow, false)
	return auth, attachHoldOutcomes(ctx, q, merchantID.UUID(), auth)
}

func validateProviderBillingRefusal(in ProviderBillingRefusalInput) error {
	if err := validateOperationID(in.OperationID); err != nil {
		return err
	}
	switch ProviderBillingQualificationReason(in.Reason) {
	case ProviderBillingLifecycleUnprovable, ProviderBillingUnavailable, ProviderBillingObservationRejected:
	default:
		return fmt.Errorf("reason must be %q, %q or %q", ProviderBillingLifecycleUnprovable, ProviderBillingUnavailable, ProviderBillingObservationRejected)
	}
	if in.Detail != "" {
		if err := validateOperationAuthorizationText("detail", in.Detail, providerBillingMaxNoteBytes); err != nil {
			return err
		}
	}
	return nil
}

func replayProviderBillingRefusal(row gen.BillingCostRefusal, in ProviderBillingRefusalInput) error {
	switch {
	case row.Reason != string(in.Reason):
		return &billing.ProviderBillingRefusalConflict{Field: "reason"}
	case providerBillingOptionalString(row.Detail) != in.Detail:
		return &billing.ProviderBillingRefusalConflict{Field: "detail"}
	}
	return nil
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

// attachHoldOutcomes loads each hold's refusal and the resolution that closed it.
func attachHoldOutcomes(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, auths ...*OperationAuthorization) error {
	if len(auths) == 0 {
		return nil
	}
	ids := make([]string, len(auths))
	byID := make(map[string]*OperationAuthorization, len(auths))
	for i, auth := range auths {
		ids[i] = auth.OperationID
		byID[auth.OperationID] = auth
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

// ListOperationAuthorizations pages the merchant's holds, newest first, each
// with its refusal and resolution. Refused true with state open lists the holds
// waiting for an operator.
func (s *MoneyService) ListOperationAuthorizations(ctx context.Context, filter billing.OperationAuthorizationListParams) (billing.ListPage[*OperationAuthorization], error) {
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
		case billing.OperationAuthorizationOpen, billing.OperationAuthorizationReleased, billing.OperationAuthorizationSettled:
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
		return attachHoldOutcomes(ctx, q, merchantID.UUID(), page.Items...)
	})
	return page, err
}

type operationAuthorizationCursor struct {
	At          time.Time `json:"t"`
	OperationID string    `json:"o"`
}
