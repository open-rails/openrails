# Cards and payment methods

How OpenRails keeps a customer's saved cards: who holds each card (its
custodian), who charges it (a PSP on a rail), the card's version history, the
agreements under which it may be charged without the customer (mandates), and
the firewall that keeps card numbers out of OpenRails (PCI SAQ A). Checkout,
renewals and dunning charge cards; this system decides which card, under which
agreement, and keeps each card true to its vault.

## Concepts

- **Payment method** (`payment_methods`): one stored card, with `rail`, `custodian`, and `psp_id` when the PSP holds it.
- **Custodian**: who holds the card: `psp` (the processor itself), `basis_theory`, `hyperswitch`, or `platform` (the deployment's vault, one partition per merchant). Each kind is a `custodians.Descriptor` in code; a merchant declares custodians of a kind.
- **`card_vault`, `charge_psps`**: merchant settings: where new cards are held, and which PSPs charge them.
- **`card_entry`**: an NMI PSP setting: `browser` (a Collect.js token, the default) or `server` (the card is posted to OpenRails).
- **Status**: `requires_action`, `active`, then `closed`, `replaced` or `removed`; chargeable when `active` with no `park_reason` (`paymentmethods.Chargeable`).
- **Card version** (`payment_method_versions`): an append-only change to a card: `saved`, `updated`, `brand_changed`, `closed` or `contact_cardholder`.
- **Mandate** (`mdt_`, `mandates`): one agreement on one card: `recurring` (a subscription), `unscheduled` (collection in one currency) or `card_on_file` (one-click reuse); `active`, `requires_reconsent`, `revoked` or `ended`.
- **Lineage**: the ids of the transaction that stored the card (`initial_transaction_id`, `network_transaction_id`), written once and cited by later charges.

## How it works

### Saving a card

`POST /v1/me/payment-methods` follows the merchant's custody:

- **NMI** (`RailPaymentMethodService.CreatePaymentMethod`): vault a Collect.js
  token (`CreateCustomerVault`), or with `card_entry: server` the `card` field
  through the `nmi_card_vault` operation, whose vault and billing ids derive
  from the operation. A `validate` that moves no money verifies it for reuse
  (`VerifyStoredCredential`); then the row, its `saved` version and the
  `card_on_file` mandate commit together (`CreateStored`). One vault per card.
- **Stripe** (`SaveStripeCard`): a SetupIntent on the PSP's own Stripe customer
  (`psp_customers`), keyed by the card's deterministic id; a bank challenge
  answers `requires_action`, finished by `POST /v1/me/payment-methods/{id}/confirm`.
- **Basis Theory or platform vault** (`SaveBasisTheoryCard`), **HyperSwitch**
  (`SaveCustodianCard`, then confirm with `ConfirmCustodianCard`).

A custodian's card has no `psp_id`: a charge no obligation binds to a PSP goes to
the one live PSP of its rail that reaches the custodian (`charge.RoutePSP`; none
or two is `ErrNoRoute`). A save left in `requires_action` is removed after 24
hours (`payment.SetupAbandonAfter`).

### Charging under a mandate

- `mandates.ForSubscription`, `ForCollection` and `Citable` find the active
  mandate for the customer, card and PSP; a merchant-initiated charge without
  one is refused (`stored_credential_required`). Lineage belongs to the PSP
  account that ran the storing transaction; another account's never stands in.
- The charge operation freezes the card and mandate; `mandates.Recheck`
  re-validates the lineage under a share lock before sending.
- One live mandate per scope (`mandates_recurring_live_key` and its twins);
  lineage is write-once (`guard_mandate_update`). A revoked agreement emits
  `mandate.revoked` ([charge agreements](../client.md#payments-and-refunds)).

### Card changes

`paymentmethods.ApplyCardLifecycle` is the one writer, for every source: Stripe
`payment_method.automatically_updated` and `.updated`, NMI Account Updater
notices, Basis Theory token events, reconcile and customer edits. Each change
is a version, deduplicated by `payment_method_versions_event_key`.

- A same-brand reissue updates the card and keeps its mandates; waiting
  memberships retry on it.
- A brand change sets its mandates `requires_reconsent` (`mandates.RequireReconsent`)
  until the customer pays with the card present.
- A closed account closes the card and ends its mandates; its memberships wait
  for another card and are never canceled.

Basis Theory's Account Updater is asked about due cards only: triggers mark
`stored_card_checks`, the hourly `openrails.stored_card_refresh_due` scan (and
an expired-card decline) queues `openrails.stored_card_refresh`, which runs an
`account_updater` operation whose id is the vendor's idempotency key.

### Removal

`DeletePaymentMethod` is refused while a subscription that is not canceled (`unverified` and `awaiting_method` included), an unresolved
operation or an in-flight charge uses the card. The customer's own removal spends the customer-action
budget (`user_destructive_reservations`); a staff or host removal is admitted as
`delete_payment_method` instead. `FencePaymentMethodDeletion` locks the card (its
`park_reason` names the operation), then a request-only operation
(`nmi_vault_delete`, `hyperswitch_method_delete` or `platform_method_delete`)
verifies, then executes. Only on the provider's confirmation is the row
`removed`; its versions stay, and an erased handle cannot be saved again.

### Card-number firewall

`cardguard.ContainsPAN` is the one detector: 13 to 19 digits in a plausible grouping, Luhn-valid and in an issued range, except one unseparated run of at most 15 digits right after a `+` (an E.164 phone number). Unknown request fields are refused, and every field of a card save, edit or confirmation and of an order create or payment is scanned before anything else (`refuseCardNumber`, `cardguard.PANField`; `400 invalid_param` naming the field), as are idempotency keys, external transaction ids and declared books; an unknown field named by a card number is refused without echoing it. The typed `card` field (`billing.Card`), the one field a card number belongs in, is admitted only on a `card_entry: server` PSP (`ErrCardEntryNotEnabled`), over HTTPS unless the merchant has no live card PSP (`cardFieldAdmitted`); it prints as `[card]`, is read only by `Unseal`, is wiped by `Zero` once NMI has it, and only brand, last four and expiry are kept.

Tables owned: `payment_methods`, `payment_method_versions`, `mandates`,
`custodians`, `psp_customers`, `stored_card_checks`.

## Interfaces

- **Go** (`*Client`): `ListPaymentMethods`, `DeletePaymentMethod`, `SetSubscriptionPaymentMethod`, `ListCustodians`, `SetCustodian`.
- **HTTP**: `/v1/me/payment-methods` (GET, POST), `/v1/me/payment-methods/{id}`
  (PATCH, DELETE), `POST /v1/me/payment-methods/{id}/confirm`,
  `PUT /v1/me/default-payment-methods/{currency}`, `GET /v1/me/agreements`,
  `POST /v1/me/agreements/{id}/revoke`,
  `GET /v1/admin/customers/{customer_id}/payment-methods`,
  `DELETE /v1/admin/customers/{customer_id}/payment-methods/{id}`,
  `GET /v1/merchant/custodians`, `PUT /v1/merchant/custodians/{key}`.
- **Configuration**: `card_vault`, `charge_psps`, a PSP's `custodian` and
  `card_entry`, the deployment's `card_vault` (`kind`, `tenant_id`, `kv_mount`),
  `user_action_rate_limit`.
- **Docs**: [custody](../client.md#merchant-configuration-and-psps), [agreements](../client.md#payments-and-refunds), [limits](../standalone.md#abuse-limits).

## Guarantees

### 1. Exactly-once provider calls

Removals, server-entered cards and Account Updater requests are
`provider_intents` operations recorded before they are sent. An uncertain
removal is re-verified, never re-sent; `nmi_card_vault` holds the card only in
the request (`cardguard.WithCard`), so no retry can send it again. NMI token
saves and verifications are direct calls that move no money; a lost reply
answers `provider_outcome_unknown`.

Checks:
- OR-J12-S71: an NMI erasure completes only after provider confirmation, with no second delete.
- `ManualPaymentMethodDeleteTypes` are excluded from `openrails.operation_wakes`, so a removal runs only on request.
- OR-J12-S01: an owned capture reference is saved and confirmed exactly once.

### 2. Agreement with providers

Every card change goes through `ApplyCardLifecycle`; a replayed event writes
nothing. An NMI Account Updater notice is a reason to read the vault. A card is
`removed` only once the provider confirms, and each Stripe account keeps its
own customer.

Checks:
- OR-J12-S13: each Stripe account charges and saves on its own customer.
- OR-J12-S08, S10: an updater answer becomes a version; a throttled vault marks nothing checked.
- OR-J12-S06: changing new-card custody keeps existing mandates, and the renewal uses the original vault and mandate.

### 3. Exact money

Does not apply: no amount is stored here, and verifications move no money (NMI `validate`, Stripe SetupIntents).

### 4. Access control and tenant isolation

Customer routes act only on the signed-in customer's cards (`ownedPaymentMethod`
answers 404 otherwise). Admin routes need `merchant:customer:read` or
`merchant:customer:manage`; custodian routes the config pair. Every query
names `merchant_id`, and cards, mandates and versions are tied by composite
keys (`mandates_payment_method_id_fkey` on merchant, customer and card).

Checks:
- OR-J12-S07: another customer, a read-only key and another merchant's key are refused, and another customer's path is 404.
- OR-J12-S02: a foreign, reused or stale card reference is refused.
- OR-J06-S06: custody configuration keeps tenant ownership and never echoes secrets.

### 5. Contracts match the code

Routes are declared in `internal/http/routes/payment_methods.go`,
`agreements.go` and `psps.go`, and snapshotted in `api/openapi.json`; `Client`
methods in `api/go.txt`; the custody CHECKs (`payment_methods_psp_custody_check`)
in `api/schema.txt`.

Checks:
- `go run ./scripts/contracts` reports no change under `api/`; OR-J12-S06 and S07 run removal over HTTP and both clients alike.

### 6. Data lifecycle

`payment_method_versions` and
`mandates` are `Permanent`; `payment_methods`, `custodians`, `psp_customers` and
`stored_card_checks` are `State`; `user_destructive_reservations` rows go after
24 hours. A removed card is a `removed` row, not a deletion. None is partitioned.

Checks:
- Each table is in `internal/retention/tables.go` and `internal/merchantarchive/owned_tables.go`.
- `payment_method_versions_payment_method_id_fkey` is `ON DELETE RESTRICT`; `CleanupPaymentMethodBestEffort` leaves a `removed` row (`DiscardPaymentMethod`), so a card's versions stay (OR-J12-S72).

### 7. Work scales with activity

Account Updater checks are marked by triggers and read by due time
(`stored_card_checks_due_at_idx`); abandoned saves through
`payment_methods_requires_action_idx`. Removals run only on request.

Checks:
- OR-J12-S10: only due cards of armed vaults are asked; OR-J12-S30: marked cards never starve a planned check.
- OR-J12-S70: a save past its 24-hour cutoff is removed without waiting for a pass.

### 8. Abuse protection

`/v1/me/payment-methods*` is in the `payment` rate-limit bucket (40 a minute) with captcha escalation. Every card save, whatever its PSP or custodian, and every order payment checks the card-testing ledger (`refuseBlockedCardAttempt`, `card_attempts_blocked`) before any provider call ([limits](../standalone.md#abuse-limits)). The firewall above refuses card numbers.

Checks:
- OR-J12-S12 and OR-J38-S36: the detector's ranges, and numeric references that are not cards.
- OR-J12-S05: no card number, security code or secret in outputs or logs.
- OR-J12-S14: a card number in any billing detail of a token save or an order payment, in a token, a return URL or a token symbol is refused, and NMI sees nothing.
- OR-J12-S15: a customer the ledger blocks is refused before HyperSwitch is asked; `CreatePaymentMethod` checks the ledger once, before choosing a branch.
- OR-J12-S16: an E.164 phone, a UUID, a Solana address and a numeric order id pass on card saves, edits, confirmations and order pays, each of which refuses a card number; no request header is stored on a card.

### 9. Destructive-action safeguards

OpenRails never removes a card on its own: a closed card ends its mandates and
asks the customer. A customer's removal spends the merchant-wide customer-action budget
(`destructive.UserActionLimiter`), a staff one a destructive admission (OR-J34-S34); it
is attributed by `origin` and `actor`, and a user-origin removal must be the
signed-in customer's (`validatePaymentMethodDeleteAuthority`).

Checks:
- OR-J12-S07: in `readonly` provider-write mode a removal sends nothing.
- OR-J12-S06: removal is refused while a subscription uses the card.
- `requestedPaymentMethodDelete` (`internal/intents/payment_method_delete_authority.go`) is the only path past the maintenance kill switch.

## Tests

- J12: OR-J12-S01 to S04, S15 (`TestPaymentMethodContracts`), S05 (`TestFinancialFailures`), S06 (`TestMerchantOwnedBasisTheoryCardLifecycle`), S07, S71 (`TestMerchantCardRemoval`), S08 to S11, S30 (`TestStoredCardRefresh`), S12, S14, S16 (`TestCardGuard`), S13 (`TestStripeCustomerPerAccount`), S70 (`TestOrderExpiryClock`).
- Custody and imports: OR-J06-S06 (`TestMerchantCustodyConfiguration`), OR-J38-S36 (`TestCardGuard`).
- Agreements: OR-J14-S06 (`TestRenewalAgreements`), OR-J17-S04 (`TestCancellationRecoveryContracts`), OR-J26-S58 (`TestUsageBillingContracts`), OR-J16-S02 (`TestDunning`: a healthy card does not revive a terminal decline).

The only package tests are `internal/integrations/basistheory/accountupdater_test.go` and `billing/card_test.go` (the card's redaction).

Gaps:

- No scenario covers a HyperSwitch capture or charge, `card_entry: server` (`nmi_card_vault`), NMI Account Updater notices, Stripe card-updater events, re-consent after a brand change, or card edits.
- No test asserts the stored-credential fields NMI receives (`initiated_by`, `stored_credential_indicator`, `initial_transaction_id`), though `nmimock` records them.
- The `payment` bucket and the customer-action budget have no scenario here.
