-- billing.payment_methods.

-- name: CreatePaymentMethod :execrows
-- A PSP-held card names its PSP; a custodian-held card names its custodian
-- and no PSP. A new method is active, or requires_action while setup_ref
-- awaits the customer, and its history starts with the card it was saved
-- with.
WITH pm AS (
INSERT INTO billing.payment_methods (
    id, merchant_id, customer_id, rail, rail_customer_ref, rail_method_ref,
    card_brand, card_last4, card_exp_month, card_exp_year,
    metadata, created_at, updated_at, psp_id,
    custodian, custodian_id, fingerprint, network_token_id, network_token_status,
    network_token_par, charge_via, status, setup_ref
) VALUES (
    $1, sqlc.arg(merchant_id)::uuid, $2, $3, NULLIF(sqlc.arg(rail_customer_ref)::text, ''), NULLIF(sqlc.arg(rail_method_ref)::text, ''),
    sqlc.narg(card_brand)::text, sqlc.narg(card_last4)::text, sqlc.narg(card_exp_month)::smallint, sqlc.narg(card_exp_year)::smallint,
    sqlc.narg(metadata),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    sqlc.narg(psp_id)::uuid,
    sqlc.arg(custodian)::text,
    sqlc.narg(custodian_id)::uuid,
    NULLIF(sqlc.arg(fingerprint)::text, ''), NULLIF(sqlc.arg(network_token_id)::text, ''),
    NULLIF(sqlc.arg(network_token_status)::text, ''), NULLIF(sqlc.arg(network_token_par)::text, ''),
    COALESCE(NULLIF(sqlc.arg(charge_via)::text, ''), 'pan_proxy'),
    CASE WHEN sqlc.narg(setup_ref)::text IS NULL THEN 'active' ELSE 'requires_action' END, sqlc.narg(setup_ref)::text
)
RETURNING *
)
INSERT INTO billing.payment_method_versions (merchant_id, customer_id, payment_method_id, source, kind, event_ref, psp_id, custodian_id,
    rail_customer_ref, rail_method_ref, card_brand, card_last4, card_exp_month, card_exp_year, fingerprint,
    network_token_id, network_token_status, network_token_par, effective_at)
SELECT merchant_id, customer_id, id, 'customer_save', 'saved', 'created', psp_id, custodian_id,
    rail_customer_ref, rail_method_ref, NULLIF(card_brand, ''), card_last4, card_exp_month, card_exp_year, fingerprint,
    network_token_id, network_token_status, network_token_par, created_at
FROM pm;

-- name: GetPaymentMethodByID :one
SELECT * FROM billing.payment_methods WHERE payment_methods.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1;

-- name: GetPaymentMethodForShare :one
-- An operation freezing the instrument reads it under a shared lock, which
-- conflicts with the custody remap's FOR UPDATE: a remap either commits first
-- (the operation freezes the new custody) or waits for the operation to
-- commit and then sees it pinning the instrument.
SELECT * FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id = sqlc.arg(id)::uuid
FOR SHARE;

-- name: ListPaymentMethodsByIDs :many
SELECT * FROM billing.payment_methods WHERE payment_methods.merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: GetCollectionCustodianAccountsForShare :one
-- Existing obligations retain the saved card's explicit custody/account, even
-- after an archive or a new default custodian. Do not re-route to p.custodian_id.
SELECT sqlc.embed(p), sqlc.embed(c)
FROM billing.psps p
JOIN billing.custodians c ON c.merchant_id = p.merchant_id
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid
  AND p.id = sqlc.arg(psp_id)::uuid
  AND c.id = sqlc.arg(custodian_id)::uuid
FOR SHARE OF p, c;

-- name: DeletePaymentMethod :execrows
DELETE FROM billing.payment_methods WHERE payment_methods.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1;

-- name: ListPaymentMethodsByCustomer :many
SELECT * FROM billing.payment_methods pm
WHERE pm.merchant_id = sqlc.arg(merchant_id)::uuid
  AND pm.customer_id = sqlc.arg(customer_id)::uuid
ORDER BY pm.created_at DESC, pm.id DESC;

-- name: ListPaymentMethodsByCustomerPage :many
-- One page of a customer's methods, newest first, after a (created_at, id)
-- cursor.
SELECT * FROM billing.payment_methods pm
WHERE pm.merchant_id = sqlc.arg(merchant_id)::uuid
  AND pm.customer_id = sqlc.arg(customer_id)::uuid
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (pm.created_at, pm.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY pm.created_at DESC, pm.id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: GetPaymentMethodByRailMethodRefForPSP :one
-- Provider webhook folds must bind the instrument to the exact account whose
-- credentials verified the event. The same merchant may run multiple accounts
-- on one rail, so a rail-only lookup is not sufficient for provider truth.
SELECT * FROM billing.payment_methods pm
WHERE pm.merchant_id = sqlc.arg(merchant_id)::uuid
  AND pm.rail = sqlc.arg(rail)
  AND pm.psp_id = sqlc.arg(psp_id)::uuid
  AND pm.custodian_id IS NOT DISTINCT FROM sqlc.narg(custodian_id)::uuid
  AND pm.rail_method_ref = sqlc.arg(rail_method_ref)::text
LIMIT 1;

-- name: LockStripePaymentState :exec
-- Serialize Stripe payment-method/default convergence across workers and
-- instances for one merchant, PSP, and provider customer.
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 0));

-- name: UpdatePaymentMethod :execrows
UPDATE billing.payment_methods SET
    customer_id = $2,
    rail = $3,
    rail_customer_ref = NULLIF(sqlc.arg(rail_customer_ref)::text, ''),
    rail_method_ref = NULLIF(sqlc.arg(rail_method_ref)::text, ''),
    card_brand = sqlc.narg(card_brand)::text,
    card_last4 = sqlc.narg(card_last4)::text,
    card_exp_month = sqlc.narg(card_exp_month)::smallint,
    card_exp_year = sqlc.narg(card_exp_year)::smallint,
    metadata = sqlc.narg(metadata),
    updated_at = sqlc.arg(updated_at)
WHERE payment_methods.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1;

-- Verified Stripe setup/provider readback can complete a historical mirror
-- missing its customer reference. It cannot rename an existing binding.
-- name: BindMissingStripeCustomerReference :execrows
UPDATE billing.payment_methods
SET rail_customer_ref=sqlc.arg(rail_customer_ref)::text, updated_at=sqlc.arg(now)::timestamptz
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid
 AND customer_id=sqlc.arg(customer_id)::uuid AND psp_id=sqlc.arg(psp_id)::uuid
 AND rail='stripe' AND rail_method_ref=sqlc.arg(rail_method_ref)::text
 AND rail_customer_ref IS NULL AND custodian='psp' AND custodian_id IS NULL AND park_reason IS NULL;

-- name: ListPaymentMethodsByRails :many
SELECT * FROM billing.payment_methods pm
WHERE pm.merchant_id = sqlc.arg(merchant_id)::uuid AND pm.rail = ANY(sqlc.arg(rails)::text[])
ORDER BY pm.created_at DESC;

-- name: ListCustomerPaymentMethodsByIDs :many
SELECT * FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[])
  AND customer_id = sqlc.arg(customer_id)::uuid
ORDER BY created_at DESC, id DESC;

-- name: ListPaymentMethodsByCustomerRails :many
SELECT * FROM billing.payment_methods pm
WHERE pm.merchant_id = sqlc.arg(merchant_id)::uuid AND pm.customer_id = $1
  AND pm.rail = ANY(sqlc.arg(rails)::text[])
ORDER BY pm.created_at DESC;

-- name: CountPaymentMethodForUser :one
SELECT count(*) FROM billing.payment_methods pm
WHERE pm.merchant_id = sqlc.arg(merchant_id)::uuid AND pm.id = $1 AND pm.customer_id = $2;

-- name: ListPaymentMethodsByRail :many
SELECT * FROM billing.payment_methods pm
WHERE pm.merchant_id = sqlc.arg(merchant_id)::uuid AND pm.rail = $1
ORDER BY pm.created_at DESC;

-- name: ListLatestChargeByPaymentMethodIDs :many
-- #589 derived health: each payment method's most recent charge attempt
-- (#1111), approved or not.
SELECT DISTINCT ON (a.payment_method_id)
    a.payment_method_id, a.attempted_at, a.category
FROM billing.payment_attempts a
WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.payment_method_id = ANY(sqlc.arg(ids)::uuid[]) AND a.kind <> 'verify'
ORDER BY a.payment_method_id, a.attempted_at DESC, a.id DESC;

-- name: CountPaymentMethodsSharingCustomerRef :one
-- #682 shared-vault guard: how many OTHER stored methods share this rail
-- customer-scope handle (e.g. an imported multi-card NMI vault) within the
-- merchant.
SELECT count(*) FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
  AND rail = $1
  AND rail_customer_ref = sqlc.arg(rail_customer_ref)::text
  AND id <> sqlc.arg(exclude_id);

-- name: GetPaymentMethodByRailInstrument :one
-- #297: resolve the stored instrument for a charge that only knows the rail
-- handles (checkout intents carry vault/billing ids, not the local row id).
-- One-vault-per-card minting (#682) makes rail_customer_ref alone decisive for
-- NMI; the method-ref predicate narrows within shared (imported) vaults and is
-- skipped when either side has no billing id.
SELECT * FROM billing.payment_methods pm
WHERE pm.merchant_id = sqlc.arg(merchant_id) AND pm.psp_id = sqlc.arg(psp_id)::uuid
  AND pm.rail = sqlc.arg(rail)
  AND pm.rail_customer_ref = sqlc.arg(rail_customer_ref)::text
  AND (pm.rail_method_ref = sqlc.arg(rail_method_ref)::text
       OR sqlc.arg(rail_method_ref)::text = ''
       OR pm.rail_method_ref IS NULL)
ORDER BY pm.created_at
LIMIT 1;

-- name: GetPaymentMethodByFingerprint :one
-- #795: dedup lookup — an intent whose fingerprint matches a stored instrument
-- reuses that instrument instead of minting a duplicate. Scoped by the
-- custodian, which issues the fingerprint and holds the card, and by the
-- customer: one customer's charge never reuses another's card or agreement.
-- A number entered again after its reissue finds the method through its
-- history.
SELECT * FROM billing.payment_methods pm
WHERE pm.merchant_id = sqlc.arg(merchant_id)
  AND pm.customer_id = sqlc.arg(customer_id)::uuid
  AND pm.custodian = sqlc.arg(custodian)
  AND pm.custodian_id = sqlc.arg(custodian_id)::uuid
  AND pm.status = 'active'
  AND (pm.fingerprint = sqlc.arg(fingerprint)::text
       OR pm.id IN (SELECT v.payment_method_id FROM billing.payment_method_versions v
                    WHERE v.merchant_id = sqlc.arg(merchant_id) AND v.customer_id = sqlc.arg(customer_id)::uuid
                      AND v.fingerprint = sqlc.arg(fingerprint)::text AND v.custodian_id = sqlc.arg(custodian_id)::uuid))
ORDER BY pm.created_at
LIMIT 1;

-- name: ParkPaymentMethodByMethodRef :many
-- #795 cancellation-last-resort: a custody-side instrument problem (token
-- deleted/expired, closed account) PARKS the instrument — charges fail loudly,
-- the operator is notified, and nothing is terminally canceled. Idempotent:
-- an already-parked instrument keeps its first park. Keyed on the custodian
-- that reported the problem (or#879), since the method ref is its token id.
UPDATE billing.payment_methods SET
    park_reason = sqlc.arg(park_reason)::text,
    parked_at = now(),
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND custodian_id = sqlc.arg(custodian_id)::uuid
  AND custodian = sqlc.arg(custodian)
  AND rail_method_ref = sqlc.arg(rail_method_ref)::text
  AND park_reason IS NULL
RETURNING id, customer_id, psp_id;

-- name: ReplacePaymentMethodCard :execrows
-- An in-place card replacement moves the method onto the verified billing
-- entry; its agreements are replaced in the same transaction.
UPDATE billing.payment_methods SET
    rail_method_ref = sqlc.arg(new_rail_method_ref)::text,
    card_brand = sqlc.narg(card_brand)::text,
    card_last4 = sqlc.narg(card_last4)::text,
    card_exp_month = sqlc.narg(card_exp_month)::smallint,
    card_exp_year = sqlc.narg(card_exp_year)::smallint,
    metadata = sqlc.narg(metadata),
    park_reason = NULL,
    parked_at = NULL,
    updated_at = sqlc.arg(updated_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND rail_method_ref = sqlc.arg(old_rail_method_ref)::text
  AND (park_reason IS NULL OR park_reason NOT LIKE 'delete:%');

-- name: ListVaultPaymentMethods :many
-- #1115: the stored cards on one NMI vault of one PSP.
SELECT * FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND rail = 'nmi'
  AND rail_customer_ref = sqlc.arg(rail_customer_ref)::text AND (park_reason IS NULL OR park_reason NOT LIKE 'delete:%')
ORDER BY created_at, id;

-- name: GetPaymentMethodForUpdate :one
SELECT * FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
FOR UPDATE;

-- name: GetPaymentMethodByPSPRefs :one
-- The payment_methods_psp_instrument_key identity: an empty ref is the stored
-- NULL, which the key treats as a value. OR, unlike IS NOT DISTINCT FROM,
-- keeps each ref an index condition.
SELECT id, customer_id, rail FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND custodian_id IS NULL
  AND (rail_customer_ref = NULLIF(sqlc.arg(rail_customer_ref)::text, '') OR (rail_customer_ref IS NULL AND sqlc.arg(rail_customer_ref)::text = ''))
  AND (rail_method_ref = NULLIF(sqlc.arg(rail_method_ref)::text, '') OR (rail_method_ref IS NULL AND sqlc.arg(rail_method_ref)::text = ''));

-- name: GetPaymentMethodByPSPRailRefs :one
-- GetPaymentMethodByPSPRefs on one rail.
SELECT id, customer_id FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND custodian_id IS NULL AND rail = sqlc.arg(rail)::text
  AND (rail_customer_ref = NULLIF(sqlc.arg(rail_customer_ref)::text, '') OR (rail_customer_ref IS NULL AND sqlc.arg(rail_customer_ref)::text = ''))
  AND (rail_method_ref = NULLIF(sqlc.arg(rail_method_ref)::text, '') OR (rail_method_ref IS NULL AND sqlc.arg(rail_method_ref)::text = ''));

-- name: CustomerHasVaultedPaymentMethod :one
SELECT EXISTS (
    SELECT 1 FROM billing.payment_methods
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND status = 'active' AND parked_at IS NULL
);

-- name: GetPaymentMethodByCustodianRef :one
-- A customer's custodian-held card by its custodian token.
SELECT * FROM billing.payment_methods pm
WHERE pm.merchant_id = sqlc.arg(merchant_id)::uuid
  AND pm.customer_id = sqlc.arg(customer_id)::uuid
  AND pm.custodian <> 'psp'
  AND pm.custodian_id = sqlc.arg(custodian_id)::uuid
  AND pm.rail_method_ref = sqlc.arg(rail_method_ref)::text
ORDER BY pm.created_at, pm.id
LIMIT 1;

-- name: SetPaymentMethodMetadata :execrows
-- The billing details the customer edited on a live card.
UPDATE billing.payment_methods SET metadata = sqlc.narg(metadata), updated_at = sqlc.arg(updated_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND status = 'active';

-- name: CompletePaymentMethodSetup :execrows
-- The customer completed the bank's authentication: the card is saved.
UPDATE billing.payment_methods SET status = 'active', setup_ref = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND status = 'requires_action';

-- name: AbandonPaymentMethodSetup :execrows
-- The bank refused the card, or its authentication was never completed.
UPDATE billing.payment_methods SET status = 'removed', setup_ref = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND status = 'requires_action';

-- name: AbandonStalePaymentMethodSetups :execrows
-- Card saves that waited for the customer past the cutoff are removed.
UPDATE billing.payment_methods SET status = 'removed', setup_ref = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND status = 'requires_action' AND created_at < sqlc.arg(before)::timestamptz;

-- name: ListStaleSetupMerchants :many
-- The sweep's work queue: merchants with a card save waiting past the cutoff.
SELECT DISTINCT merchant_id FROM billing.payment_methods
WHERE status = 'requires_action' AND created_at < sqlc.arg(before)::timestamptz
LIMIT sqlc.arg(merchant_limit)::int;
