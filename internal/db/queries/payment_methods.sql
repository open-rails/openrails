-- billing.payment_methods.

-- name: CreatePaymentMethod :execrows
-- A PSP-held card names its PSP; a custodian-held card names its custodian
-- and no PSP.
INSERT INTO billing.payment_methods (
    id, merchant_id, customer_id, rail, rail_customer_ref, rail_method_ref,
    card_brand, card_last4, card_exp_month, card_exp_year,
    metadata, created_at, updated_at, psp_id,
    custodian, custodian_id, fingerprint, network_token_id, network_token_status,
    network_token_par, charge_via, stored_credential_recurring_ref
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
    NULLIF(sqlc.arg(stored_credential_recurring_ref)::text, '')
);

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

-- name: CaptureStoredCredentialRef :execrows
-- #297: persist the rail-scoped stored-credential replay reference captured by
-- a successful charge, WRITE-ONCE per agreement type — an existing non-empty
-- reference is never overwritten (the sequence anchors on its first capture).
UPDATE billing.payment_methods SET
    stored_credential_recurring_ref = CASE
        WHEN sqlc.arg(agreement)::text = 'recurring' THEN NULLIF(sqlc.arg(ref)::text, '')
        ELSE stored_credential_recurring_ref END,
    stored_credential_unscheduled_ref = CASE
        WHEN sqlc.arg(agreement)::text = 'unscheduled' THEN NULLIF(sqlc.arg(ref)::text, '')
        ELSE stored_credential_unscheduled_ref END,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)
  AND id = sqlc.arg(id)
  AND ((sqlc.arg(agreement)::text = 'recurring' AND stored_credential_recurring_ref IS NULL)
    OR (sqlc.arg(agreement)::text = 'unscheduled' AND stored_credential_unscheduled_ref IS NULL));

-- name: CaptureStoredCredentialRefByRailInstrument :execrows
-- #297: instrument-handle variant of CaptureStoredCredentialRef for charge
-- sites that never load the local row (checkout sale/subscription intents).
-- Same write-once semantics.
UPDATE billing.payment_methods SET
    stored_credential_recurring_ref = CASE
        WHEN sqlc.arg(agreement)::text = 'recurring' THEN NULLIF(sqlc.arg(ref)::text, '')
        ELSE stored_credential_recurring_ref END,
    stored_credential_unscheduled_ref = CASE
        WHEN sqlc.arg(agreement)::text = 'unscheduled' THEN NULLIF(sqlc.arg(ref)::text, '')
        ELSE stored_credential_unscheduled_ref END,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id) AND psp_id = sqlc.arg(psp_id)::uuid
  AND rail = sqlc.arg(rail)
  AND rail_customer_ref = sqlc.arg(rail_customer_ref)::text
  AND (rail_method_ref = sqlc.arg(rail_method_ref)::text
       OR sqlc.arg(rail_method_ref)::text = ''
       OR rail_method_ref IS NULL)
  AND ((sqlc.arg(agreement)::text = 'recurring' AND stored_credential_recurring_ref IS NULL)
    OR (sqlc.arg(agreement)::text = 'unscheduled' AND stored_credential_unscheduled_ref IS NULL));

-- name: GetPaymentMethodByFingerprint :one
-- #795: dedup lookup — an intent whose fingerprint matches a stored instrument
-- reuses that instrument instead of minting a duplicate. Scoped by the
-- custodian, which issues the fingerprint and holds the card.
SELECT * FROM billing.payment_methods pm
WHERE pm.merchant_id = sqlc.arg(merchant_id)
  AND pm.custodian = sqlc.arg(custodian)
  AND pm.custodian_id = sqlc.arg(custodian_id)::uuid
  AND pm.fingerprint = sqlc.arg(fingerprint)::text
ORDER BY pm.created_at
LIMIT 1;

-- name: SetPaymentMethodNetworkToken :execrows
-- #795 NT provisioning result (id/status/par). Never touches PAN-side expiry.
UPDATE billing.payment_methods SET
    network_token_id = NULLIF(sqlc.arg(network_token_id)::text, ''),
    network_token_status = NULLIF(sqlc.arg(network_token_status)::text, ''),
    network_token_par = NULLIF(sqlc.arg(network_token_par)::text, ''),
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id) AND id = sqlc.arg(id)
  AND (park_reason IS NULL OR park_reason NOT LIKE 'delete:%');

-- name: SetNetworkTokenStatusByNetworkTokenID :execrows
-- #795 webhook fold: NT lifecycle status/enrichment only (idempotent). Keyed on
-- the CUSTODIAN that sent the event (or#879) — the network token is a custody
-- artefact, and the rail says nothing about who minted it.
UPDATE billing.payment_methods SET
    network_token_status = sqlc.arg(network_token_status)::text,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND custodian <> 'psp'
  AND custodian_id = sqlc.arg(custodian_id)::uuid
  AND custodian = sqlc.arg(custodian)
  AND network_token_id = sqlc.arg(network_token_id)::text
  AND network_token_status IS DISTINCT FROM sqlc.arg(network_token_status)::text;

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

-- name: ParkStripePaymentMethodByRef :execrows
-- A Stripe detach is irreversible provider truth. Preserve the local evidence,
-- but make the exact PSP-owned instrument unusable, within the merchant.
UPDATE billing.payment_methods SET
    park_reason = sqlc.arg(park_reason)::text,
    parked_at = COALESCE(parked_at, now()),
    updated_at = now()
WHERE payment_methods.merchant_id = sqlc.arg(merchant_id)::uuid AND rail = 'stripe'
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND rail_method_ref = sqlc.arg(rail_method_ref)::text
  AND park_reason IS NULL;

-- name: RotateCustodianMethodRef :many
-- #795 Account Updater UPD_* fold: the custodian minted a NEW token id —
-- re-point rail_method_ref and refresh card metadata. The old->new mapping is
-- the same machinery a future custodian swap remap uses.
-- or#872 (do not fight the updater): the park is CLEARED here. An UPD_* row is
-- the network telling us the credential was reissued, so an instrument parked
-- earlier for bt_token_expired / bt_au_closed_account is usable again; leaving
-- the park set kept charges refused (custodian_proxy_collection) and invoice
-- recovery skipping the method, which is the engine overruling the very
-- recovery the account updater exists to deliver.
UPDATE billing.payment_methods SET
    rail_method_ref = sqlc.arg(new_method_ref)::text,
    fingerprint = COALESCE(NULLIF(sqlc.arg(new_fingerprint)::text, ''), fingerprint),
    card_brand = COALESCE(sqlc.narg(card_brand)::text, card_brand),
    card_last4 = COALESCE(sqlc.narg(card_last4)::text, card_last4),
    card_exp_month = COALESCE(sqlc.narg(card_exp_month)::smallint, card_exp_month),
    card_exp_year = COALESCE(sqlc.narg(card_exp_year)::smallint, card_exp_year),
    park_reason = NULL,
    parked_at = NULL,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND custodian_id = sqlc.arg(custodian_id)::uuid
  AND custodian = sqlc.arg(custodian)
  AND rail_method_ref = sqlc.arg(old_method_ref)::text
  AND (park_reason IS NULL OR park_reason NOT LIKE 'delete:%')
RETURNING id, customer_id, psp_id;

-- name: RefreshCustodianCardMetadata :execrows
-- #795 token.updated fold: refresh masked metadata from the custodian's read.
UPDATE billing.payment_methods SET
    card_brand = COALESCE(sqlc.narg(card_brand)::text, card_brand),
    card_last4 = COALESCE(sqlc.narg(card_last4)::text, card_last4),
    card_exp_month = COALESCE(sqlc.narg(card_exp_month)::smallint, card_exp_month),
    card_exp_year = COALESCE(sqlc.narg(card_exp_year)::smallint, card_exp_year),
    fingerprint = COALESCE(NULLIF(sqlc.arg(fingerprint)::text, ''), fingerprint),
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND custodian_id = sqlc.arg(custodian_id)::uuid
  AND custodian = sqlc.arg(custodian)
  AND rail_method_ref = sqlc.arg(rail_method_ref)::text;

-- name: ReplacePaymentMethodCard :execrows
-- An in-place card replacement moves the method onto the verified billing
-- entry: card metadata and its recurring agreement change together.
UPDATE billing.payment_methods SET
    rail_method_ref = sqlc.arg(new_rail_method_ref)::text,
    card_brand = sqlc.narg(card_brand)::text,
    card_last4 = sqlc.narg(card_last4)::text,
    card_exp_month = sqlc.narg(card_exp_month)::smallint,
    card_exp_year = sqlc.narg(card_exp_year)::smallint,
    metadata = sqlc.narg(metadata),
    stored_credential_recurring_ref = NULLIF(sqlc.arg(recurring_ref)::text, ''),
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

-- name: RefreshPaymentMethodCard :execrows
-- #1115: an account updater reissued the card in place. Its details change,
-- and a park an earlier notice set is cleared.
UPDATE billing.payment_methods SET
    card_brand = COALESCE(sqlc.narg(card_brand)::text, card_brand),
    card_last4 = COALESCE(sqlc.narg(card_last4)::text, card_last4),
    card_exp_month = COALESCE(sqlc.narg(card_exp_month)::smallint, card_exp_month),
    card_exp_year = COALESCE(sqlc.narg(card_exp_year)::smallint, card_exp_year),
    park_reason = NULL,
    parked_at = NULL,
    updated_at = sqlc.arg(updated_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND (park_reason IS NULL OR park_reason NOT LIKE 'delete:%');

-- name: ParkPaymentMethod :execrows
-- #1115: an account updater reported the card's account closed. The first
-- park stands.
UPDATE billing.payment_methods SET
    park_reason = sqlc.arg(park_reason)::text,
    parked_at = sqlc.arg(parked_at)::timestamptz,
    updated_at = sqlc.arg(parked_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND park_reason IS NULL;

-- name: InsertPaymentMethodUpdate :exec
-- #1115: idempotent on (source, event_ref, method); occurred_at defaults to now.
INSERT INTO billing.payment_method_updates (merchant_id, payment_method_id, customer_id, psp_id, source, kind, event_ref, occurred_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(payment_method_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.narg(psp_id)::uuid,
    sqlc.arg(source)::text, sqlc.arg(kind)::text, sqlc.arg(event_ref)::text, COALESCE(sqlc.narg(occurred_at)::timestamptz, now()))
ON CONFLICT DO NOTHING;

-- name: GetPaymentMethodByPSPRefs :one
SELECT id, customer_id, rail FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
  AND rail_customer_ref = sqlc.arg(rail_customer_ref)::text AND rail_method_ref = sqlc.arg(rail_method_ref)::text;

-- name: GetPaymentMethodByPSPRailRefs :one
SELECT id, customer_id FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND rail = sqlc.arg(rail)::text
  AND rail_customer_ref = sqlc.arg(rail_customer_ref)::text AND rail_method_ref = sqlc.arg(rail_method_ref)::text;

-- name: CustomerHasVaultedPaymentMethod :one
SELECT EXISTS (
    SELECT 1 FROM billing.payment_methods
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND parked_at IS NULL
);

-- name: ListCustodianRoutePSPs :many
-- The live PSPs of a rail that reach a custodian, in its environment: the
-- PSPs a card it holds can be charged through. Two rows mean routing has no
-- single answer.
SELECT p.id FROM billing.psps p
JOIN billing.custodians c ON c.merchant_id = p.merchant_id AND c.id = p.custodian_id AND c.environment = p.environment
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.rail = sqlc.arg(rail)::text
  AND p.custodian_id = sqlc.arg(custodian_id)::uuid AND NOT p.archived
ORDER BY p.created_at, p.id
LIMIT 2;

-- name: GetPaymentMethodByCustodianRef :one
-- A custodian-held card by its custodian token.
SELECT * FROM billing.payment_methods pm
WHERE pm.merchant_id = sqlc.arg(merchant_id)::uuid
  AND pm.custodian <> 'psp'
  AND pm.custodian_id = sqlc.arg(custodian_id)::uuid
  AND pm.rail_method_ref = sqlc.arg(rail_method_ref)::text
ORDER BY pm.created_at, pm.id
LIMIT 1;
