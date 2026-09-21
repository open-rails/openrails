package intents

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/pkg/merchant"
)

func deletionMethodUnused(ctx context.Context, q *gen.Queries, mid, id uuid.UUID, accepted int64) error {
	subscriptions, err := q.ListSubscriptionsByPaymentMethodIDs(ctx, gen.ListSubscriptionsByPaymentMethodIDsParams{MerchantID: mid, PaymentMethodIds: []uuid.UUID{id}})
	if err != nil {
		return err
	}
	for _, s := range subscriptions {
		if string(s.Status) == string(models.StatusActive) || string(s.Status) == string(models.StatusPending) || string(s.Status) == string(models.StatusPastDue) {
			return paymentmethods.ErrPaymentMethodInUse
		}
	}
	n, err := q.CountUnresolvedOperationsNamingPaymentMethod(ctx, gen.CountUnresolvedOperationsNamingPaymentMethodParams{MerchantID: mid, PaymentMethodID: id})
	if err != nil {
		return err
	}
	if n > accepted {
		return paymentmethods.ErrPaymentMethodInUse
	}
	n, err = q.CountInFlightChargeIntentsForPaymentMethod(ctx, gen.CountInFlightChargeIntentsForPaymentMethodParams{MerchantID: mid, PaymentMethodID: id})
	if err != nil {
		return err
	}
	if n > 0 {
		return paymentmethods.ErrPaymentMethodInUse
	}
	return nil
}

func sameDeletionTarget(row gen.OpenrailsPaymentMethod, pm *models.PaymentMethod) bool {
	return row.ID == pm.ID && row.CustomerID == pm.CustomerID && row.PspID == pm.PspID && row.Custodian == pm.Custodian && row.CustodianID != nil && pm.CustodianID != nil && *row.CustodianID == *pm.CustodianID && row.RailMethodRef == pm.RailMethodRef && row.RailCustomerRef == pm.RailCustomerRef
}

func (h *HyperSwitchMethodDeleteHandler) admit(ctx context.Context, store *Store, pm *models.PaymentMethod) (gen.OpenrailsRailIntent, error) {
	var operation gen.OpenrailsRailIntent
	mid, err := merchant.Require(ctx)
	if err != nil {
		return operation, err
	}
	if pm == nil || pm.CustomerID == uuid.Nil || pm.Custodian != models.CustodianHyperSwitch || pm.CustodianID == nil || pm.RailCustomerRef == "" || pm.RailMethodRef == "" || h.Rails == nil {
		return operation, paymentmethods.ErrPaymentMethodDeleteUnsafe
	}
	origin, actor := paymentMethodDeleteAuthority(ctx, pm.CustomerID)
	key := TypeHyperSwitchMethodDelete + ":" + pm.ID.String()
	err = h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: pm.CustomerID}); err != nil {
			return err
		}
		scoped := store.withTxDB(d)
		prior, err := scoped.GetByIdempotencyKey(ctx, key)
		if err == nil {
			p, err := DecodeHyperSwitchMethodDelete(prior)
			if err != nil {
				return err
			}
			if p.CustomerID != pm.CustomerID || p.PaymentMethodID != pm.ID {
				return paymentmethods.ErrPaymentMethodDeleteUnsafe
			}
			operation = prior
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		handle := paymentmethods.CustodianHandle{Custodian: *pm.CustodianID, Method: pm.RailMethodRef}
		if err := paymentmethods.LockCustodianHandles(ctx, q, mid.UUID(), handle); err != nil {
			return err
		}
		current, err := q.LockPaymentMethodForCustodyRemap(ctx, gen.LockPaymentMethodForCustodyRemapParams{MerchantID: mid.UUID(), ID: pm.ID})
		if err != nil {
			return err
		}
		if !sameDeletionTarget(current, pm) || current.NetworkTokenID != "" || current.ChargeVia != "pan_proxy" {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		if err := paymentmethods.RequireCustodianHandleAvailable(ctx, q, mid.UUID(), handle); err != nil {
			return err
		}
		if err := deletionMethodUnused(ctx, q, mid.UUID(), pm.ID, 0); err != nil {
			return err
		}
		aliases, err := q.CountCustodianMethodAliases(ctx, gen.CountCustodianMethodAliasesParams{MerchantID: mid.UUID(), CustodianID: *pm.CustodianID, MethodRef: pm.RailMethodRef, CustomerID: pm.CustomerID})
		if err != nil {
			return err
		}
		if aliases.ForeignPayers != 0 || aliases.Total == 0 {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		account, err := q.LockCustodianDeletionAccount(ctx, gen.LockCustodianDeletionAccountParams{MerchantID: mid.UUID(), ID: *pm.CustodianID})
		if err != nil {
			return err
		}
		binding, err := deletionBinding(account, h.Rails.Config)
		if err != nil {
			return err
		}
		operation, err = scoped.enqueue(ctx, EnqueueParams{MerchantID: mid.UUID(), Provider: models.CustodianHyperSwitch, CustodianID: *pm.CustodianID, IntentType: TypeHyperSwitchMethodDelete, IdempotencyKey: key, Origin: origin, Actor: actor, OriginReason: "user payment-method delete", NextAttemptAt: h.Clock.Now().UTC(), Payload: HyperSwitchMethodDeletePayload{CustomerID: pm.CustomerID, PaymentMethodID: pm.ID, DetachOnly: aliases.Total > 1, Instrument: charge.FreezeInstrument(current), Binding: binding, Environment: account.Environment}})
		if err != nil {
			return err
		}
		if _, err := DecodeHyperSwitchMethodDelete(operation); err != nil {
			return err
		}
		n, err := q.FencePaymentMethodDeletion(ctx, gen.FencePaymentMethodDeletionParams{MerchantID: mid.UUID(), ID: pm.ID, OperationID: operation.ID, Now: h.Clock.Now().UTC()})
		if err != nil {
			return err
		}
		if n != 1 {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		return nil
	})
	return operation, err
}

func (h *HyperSwitchMethodDeleteHandler) checkFence(ctx context.Context, in gen.OpenrailsRailIntent, p HyperSwitchMethodDeletePayload) error {
	return h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: in.MerchantID, ID: p.CustomerID}); err != nil {
			return err
		}
		handle := paymentmethods.CustodianHandle{Custodian: *in.CustodianID, Method: p.Instrument.RailMethodRef}
		if err := paymentmethods.LockCustodianHandles(ctx, q, in.MerchantID, handle); err != nil {
			return err
		}
		row, err := q.LockPaymentMethodForCustodyRemap(ctx, gen.LockPaymentMethodForCustodyRemapParams{MerchantID: in.MerchantID, ID: p.PaymentMethodID})
		if err != nil {
			return err
		}
		if row.CustomerID != p.CustomerID || row.ParkReason != "delete:"+in.ID.String() || p.Instrument.Matches(row, charge.AgreementUnscheduled) != nil {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		aliases, err := q.CountCustodianMethodAliases(ctx, gen.CountCustodianMethodAliasesParams{MerchantID: in.MerchantID, CustodianID: *in.CustodianID, MethodRef: p.Instrument.RailMethodRef, CustomerID: p.CustomerID})
		if err != nil {
			return err
		}
		if aliases.ForeignPayers != 0 || (!p.DetachOnly && aliases.Total != 1) || (p.DetachOnly && aliases.Total < 2) {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		return deletionMethodUnused(ctx, q, in.MerchantID, p.PaymentMethodID, 1)
	})
}
