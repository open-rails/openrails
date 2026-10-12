package merchantarchive

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
)

// Provider identity is financial history. Schema foreign keys retain the rows;
// these checks also bind their frozen JSON and payer to the same merchant book.
// An accepted provider alias can describe its line differently from checkout;
// match the financial fields admission compares, while preserving both texts
// and the provider's separately accepted service-period evidence.
var providerBillReferenceChecks = []rowCheck{
	{"payments", `order_id IS NULL OR NOT EXISTS (
 SELECT 1 FROM billing.orders o WHERE o.merchant_id=$1 AND o.id=payments.order_id
 AND o.customer_id=payments.customer_id AND o.currency=payments.currency)
 OR (refunded_payment_id IS NOT NULL AND NOT EXISTS (
 SELECT 1 FROM billing.payments p WHERE p.merchant_id=$1 AND p.id=payments.refunded_payment_id
 AND p.customer_id=payments.customer_id AND p.order_id=payments.order_id AND p.currency=payments.currency))`},
	{"orders", `(collection_owner<>'engine' AND psp_id IS NULL)
 OR (payment_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM billing.payments p
 WHERE p.merchant_id=$1 AND p.id=orders.payment_id AND p.order_id=orders.id
 AND p.customer_id=orders.customer_id AND p.currency=orders.currency))`},
	{"provider_bills", `NOT EXISTS (SELECT 1 FROM billing.orders o WHERE o.merchant_id=$1
 AND o.id=provider_bills.order_id AND o.customer_id=provider_bills.customer_id AND o.psp_id=provider_bills.psp_id
 AND o.customer_id::text=snapshot->>'CustomerID' AND o.psp_id::text=snapshot->>'PSPID'
 AND o.collection_owner=snapshot->>'Owner' AND o.currency=snapshot->>'Currency' AND o.total::text=snapshot->>'Total'

 AND (o.subscription_id IS NULL OR o.subscription_id::text=snapshot->>'SubscriptionID'))
 OR source_kind IS DISTINCT FROM snapshot->>'SourceKind' OR source_key IS DISTINCT FROM snapshot->>'SourceKey'
 OR provider_reference IS DISTINCT FROM snapshot->>'ProviderReference'
 OR (snapshot->>'SubscriptionID' IS NOT NULL AND NOT EXISTS (SELECT 1 FROM billing.subscriptions s
 WHERE s.merchant_id=$1 AND s.id::text=snapshot->>'SubscriptionID' AND s.customer_id=provider_bills.customer_id AND s.psp_id=provider_bills.psp_id))
 OR (SELECT count(*) FROM billing.order_lines l WHERE l.merchant_id=$1 AND l.order_id=provider_bills.order_id)
 <> jsonb_array_length(CASE WHEN jsonb_typeof(snapshot->'Lines')='array' THEN snapshot->'Lines' ELSE '[]'::jsonb END)
 OR EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(snapshot->'Lines')='array' THEN snapshot->'Lines' ELSE '[]'::jsonb END) WITH ORDINALITY AS x(line, position)
 WHERE NOT EXISTS (SELECT 1 FROM billing.order_lines l WHERE l.merchant_id=$1 AND l.order_id=provider_bills.order_id
 AND l.customer_id=provider_bills.customer_id AND l.position=x.position-1
 AND l.price_id::text IS NOT DISTINCT FROM x.line->>'PriceID' AND l.product_id::text IS NOT DISTINCT FROM x.line->>'ProductID'
 AND COALESCE(l.quantity,1)::text=COALESCE(x.line->>'Quantity','1') AND l.unit_amount::text=x.line->>'UnitAmount' AND l.amount::text=x.line->>'Amount'
 AND COALESCE(l.source,'')=x.line->>'Source'))`},
	{"provider_schedules", `NOT EXISTS (SELECT 1 FROM billing.subscriptions s WHERE s.merchant_id=$1
 AND s.id=provider_schedules.subscription_id AND s.customer_id=provider_schedules.customer_id AND s.psp_id=provider_schedules.psp_id)
 OR (initial_order_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM billing.orders o WHERE o.merchant_id=$1 AND o.id=provider_schedules.initial_order_id
 AND o.customer_id=provider_schedules.customer_id AND o.psp_id=provider_schedules.psp_id
 AND o.collection_owner=accepted_bill->>'Owner' AND o.currency=accepted_bill->>'Currency'))
 OR subscription_id::text IS DISTINCT FROM accepted_bill->>'SubscriptionID'
 OR customer_id::text IS DISTINCT FROM accepted_bill->>'CustomerID' OR psp_id::text IS DISTINCT FROM accepted_bill->>'PSPID'
 OR EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(accepted_bill->'Lines')='array' THEN accepted_bill->'Lines' ELSE '[]'::jsonb END) x(line)
 WHERE NOT EXISTS (SELECT 1 FROM billing.prices p JOIN billing.products pr ON pr.merchant_id=$1 AND pr.id=p.product_id
 WHERE p.merchant_id=$1 AND p.id::text=x.line->>'PriceID' AND pr.id::text=x.line->>'ProductID'))`},
	{"provider_cycle_receipts", `NOT EXISTS (SELECT 1 FROM billing.provider_schedules s WHERE s.merchant_id=$1
 AND s.subscription_id=provider_cycle_receipts.subscription_id AND s.psp_id=provider_cycle_receipts.psp_id
 AND s.source_reference=provider_cycle_receipts.schedule_source_reference
 AND s.accepted_bill->>'Currency'=provider_cycle_receipts.currency
 AND s.accepted_bill->>'Total'=provider_cycle_receipts.amount::text
 AND provider_cycle_receipts.due_at>=s.first_due_at
 AND ((s.cadence_hours>0 AND mod(extract(epoch FROM (provider_cycle_receipts.due_at-s.first_due_at)),NULLIF(s.cadence_hours,0)::numeric*3600)=0)
 OR (s.cadence_months>0
 AND mod((extract(year FROM provider_cycle_receipts.due_at)-extract(year FROM s.first_due_at))*12+extract(month FROM provider_cycle_receipts.due_at)-extract(month FROM s.first_due_at),NULLIF(s.cadence_months,0))=0
 AND provider_cycle_receipts.due_at = s.first_due_at + make_interval(months => ((extract(year FROM provider_cycle_receipts.due_at)-extract(year FROM s.first_due_at))*12+extract(month FROM provider_cycle_receipts.due_at)-extract(month FROM s.first_due_at))::integer))))`},
}

func validateProviderBillReferences(ctx context.Context, tx pgx.Tx, id billing.MerchantID) error {
	for _, check := range providerBillReferenceChecks {
		if err := refuseRows(ctx, tx, id, check.table, check.predicate); err != nil {
			return err
		}
	}
	return nil
}
