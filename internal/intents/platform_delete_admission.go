package intents

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
)

func (h *PlatformMethodDeleteHandler) admit(ctx context.Context, store *Store, pm *models.PaymentMethod) (gen.BillingProviderIntent, error) {
	var operation gen.BillingProviderIntent
	mid, err := merchant.Require(ctx)
	if err != nil {
		return operation, err
	}
	if pm == nil || pm.CustomerID == uuid.Nil || !models.BasisTheoryCustody(pm.Custodian) || pm.CustodianID == nil || pm.RailCustomerRef != "" || pm.RailMethodRef == "" || h.Rails == nil {
		return operation, paymentmethods.ErrPaymentMethodDeleteUnsafe
	}
	origin, actor := paymentMethodDeleteAuthority(ctx, pm.CustomerID)
	key := manualMethodDeleteKey(TypePlatformMethodDelete, pm.ID)
	err = h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: pm.CustomerID}); err != nil {
			return err
		}
		scoped := store.withTxDB(d)
		prior, err := scoped.GetByIdempotencyKey(ctx, key)
		if err == nil {
			p, err := DecodePlatformMethodDelete(prior)
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
		if !sameDeletionTarget(current, pm) || current.NetworkTokenID != nil || current.WalletCardID != nil || current.ChargeVia != "pan_proxy" || !slices.Contains([]string{"active", "closed", "replaced", "removed"}, current.Status) {
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
		if aliases.ForeignPayers != 0 || aliases.Total != 1 {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		if _, err := q.LockCustodianDeletionAccount(ctx, gen.LockCustodianDeletionAccountParams{MerchantID: mid.UUID(), ID: *pm.CustodianID}); err != nil {
			return err
		}
		configuration := merchants.Of(h.DB)
		if configuration == nil {
			return merchants.ErrConfigUnavailable
		}
		account, ok, err := configuration.CustodianScopeByID(ctx, mid, *pm.CustodianID)
		if err != nil {
			return err
		}
		if !ok || account.Archived || account.Kind != pm.Custodian {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		tenant := account.AccountID
		if pm.Custodian == models.CustodianPlatform {
			tenant, ok = strings.CutSuffix(account.AccountID, "/"+mid.UUID().String())
		}
		if !ok || tenant == "" || strings.Contains(tenant, "/") {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		operation, err = scoped.enqueue(ctx, EnqueueParams{MerchantID: mid.UUID(), Provider: pm.Custodian, CustodianID: *pm.CustodianID, IntentType: TypePlatformMethodDelete, IdempotencyKey: key, Origin: origin, Actor: actor, OriginReason: "user payment-method delete", NextAttemptAt: h.Clock.Now().UTC(), Inline: true, Payload: PlatformMethodDeletePayload{CustomerID: pm.CustomerID, PaymentMethodID: pm.ID, Instrument: charge.FreezeCustody(current), TenantID: tenant, Environment: account.Environment}})
		if err != nil {
			return err
		}
		if _, err := DecodePlatformMethodDelete(operation); err != nil {
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

func (h *PlatformMethodDeleteHandler) checkFence(ctx context.Context, in gen.BillingProviderIntent, p PlatformMethodDeletePayload) error {
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
		if row.CustomerID != p.CustomerID || models.DerefStr(row.ParkReason) != "delete:"+in.ID.String() || p.Instrument.Matches(row) != nil || row.NetworkTokenID != nil || row.WalletCardID != nil || row.ChargeVia != "pan_proxy" {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		aliases, err := q.CountCustodianMethodAliases(ctx, gen.CountCustodianMethodAliasesParams{MerchantID: in.MerchantID, CustodianID: *in.CustodianID, MethodRef: p.Instrument.RailMethodRef, CustomerID: p.CustomerID})
		if err != nil {
			return err
		}
		if aliases.ForeignPayers != 0 || aliases.Total != 1 {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		return deletionMethodUnused(ctx, q, in.MerchantID, p.PaymentMethodID, 1)
	})
}
