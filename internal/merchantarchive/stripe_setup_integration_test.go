//go:build integration

package merchantarchive

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestStripeSetupArchiveRetainsOwnedConsentAndMethod(t *testing.T) {
	source, target := archiveDB(t, "openrails"), archiveDB(t, "stripe_setup_archive")
	mid := merchant.ID(uuid.New())
	provision(t, source, mid)
	provision(t, target, mid)
	seedBook(t, source, mid)
	psp, method, session := uuid.New(), uuid.New(), uuid.New()
	ctx := merchant.WithID(t.Context(), mid)
	state := fmt.Sprintf(`{"kind":"stripe_engine_setup","customer_ref":"cus_setup","consent":"save_for_agreed_off_session_payments_v1","payment_method_id":%q}`, method.String())
	require.NoError(t, source.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO openrails.psps(merchant_id,id,rail,account_id,environment) VALUES($1,$2,'stripe',$2::uuid::text,'test')`, mid.UUID(), psp)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO openrails.payment_methods(merchant_id,id,customer_id,psp_id,rail,rail_customer_ref,rail_method_ref,initial_transaction_id) SELECT $1,$2,id,$3,'stripe','cus_setup','pm_setup','' FROM openrails.customers WHERE merchant_id=$1 LIMIT 1`, mid.UUID(), method, psp)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO openrails.checkout_sessions(merchant_id,id,customer_id,psp_id,mode,rail,status,reference,rail_state,expires_at) SELECT $1,$2,id,$3,'payment_method','stripe','succeeded','seti_setup',$4::jsonb,now()+interval '1 hour' FROM openrails.customers WHERE merchant_id=$1 LIMIT 1`, mid.UUID(), session, psp, state)
		return err
	}))
	var artifact bytes.Buffer
	require.NoError(t, Export(t.Context(), source, mid, &artifact))
	_, err := Restore(t.Context(), target, mid, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err)
	var restored bytes.Buffer
	require.NoError(t, Export(t.Context(), target, mid, &restored))
	require.Equal(t, artifact.String(), restored.String())
	require.NoError(t, source.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE openrails.checkout_sessions SET rail_state=rail_state||'{"consent":"unaccepted"}'::jsonb WHERE id=$1`, session)
		return err
	}))
	var invalid bytes.Buffer
	require.Error(t, Export(t.Context(), source, mid, &invalid))
}
