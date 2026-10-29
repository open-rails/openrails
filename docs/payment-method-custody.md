# Payment-method custody

A stored card has two independent owners, and OpenRails records them separately.

| Axis | Question | Where it lives |
|---|---|---|
| **Processor** | Who *charges* the card? | `payment_methods.rail`; `payment_methods.psp_id` when the PSP holds it |
| **Custodian** | Who *holds* the card? | `payment_methods.custodian` |

They are orthogonal. The card can sit inside the processor that charges it
(Stripe, an NMI gateway vault) or in a neutral third-party vault that proxies
the PAN to the selected processor (Basis Theory and HyperSwitch on NMI). Custody is not a property
of the gateway, so it does not belong in the rail value.

`custodian` is never empty. "Stored at the processor itself" is the stated
value `psp`, not an absence — a DB CHECK enforces the vocabulary.

`psp_id` is set exactly when `custodian = 'psp'` (CHECK
`payment_methods_psp_custody_check`). A card a third-party custodian holds belongs
to no PSP: each charge routes to the one live PSP of the card's rail that
reaches the custodian (`charge.RoutePSP`); none, or two, is `ErrNoRoute`. An
obligation that names its PSP (a subscription, a frozen purchase instrument)
charges through that PSP, provided it still reaches the custodian.

## The matrix

| Custodian | Processor (rail) | Instrument handle | Real today |
|---|---|---|---|
| `psp` | `stripe` | `pm_…` on a Stripe Customer (`rail_method_ref`) | yes |
| `psp` | `nmi` | `customer_vault_id` (`rail_customer_ref`) | yes |
| `basis_theory` | `nmi` | merchant-owned BT card-token id (`rail_method_ref`) | verified save, one-time/subscription orders, engine renewals on the original gateway and confirmed unused-card removal; live vendor qualification pending |
| `basis_theory` | `stripe` | BT token proxied to Stripe | not built |
| `hyperswitch` | `nmi` | persistent HyperSwitch method (`rail_method_ref`) | yes |
| `platform` | `nmi` | the platform vault's token in the merchant's partition (`rail_method_ref`) | one-time and subscription orders; engine renewals on the original account; live vendor qualification pending |
| JusPay / Spreedly | any | provider token | not built |
| — no row — | `ccbill` | CCBill owns the subscription; no instrument reaches us | yes |
| — no row — | `solana` | an on-chain delegation, not a stored instrument | yes |

### "No stored instrument" is the absence of a row

CCBill and Solana never produce a `payment_methods` row. CCBill owns the
subscription end to end and rebills it itself; Solana subscriptions ride a
wallet delegation that our cranker pulls against. Neither hands OpenRails an
instrument to hold, so there is nothing to record a custodian *for*.

This is deliberate: a `payment_methods` row **is** the record of a held
instrument, so a `none`/`""` custodian would be a row asserting that it
describes nothing. Ask "is there a stored instrument?" by looking for the row
(`subscriptions.payment_method_id IS NULL`), never by reading a custody value.

## Custody vs charge routing

`custodian` says *where the card lives*. `charge_via` says *how the credential
is presented to the network* at charge time:

| `charge_via` | Meaning |
|---|---|
| `pan_proxy` | the vault detokenizes the FPAN through its proxy into the gateway request |
| `network_token` | a DPAN (network token) is presented instead of the PAN |

A Basis-Theory-held card can be charged either way, so the two fields stay
separate. `payments.token_type` records which form actually went out
(`pan_via_proxy` / `network_token` / `psp_token`).

## Where new cards go: `card_vault` and `charge_psps`

Two merchant settings choose where the cards it saves from now on are held, and which
PSPs charge new cards:

| Setting | Meaning |
|---|---|
| `card_vault` | where new cards are saved: a card PSP's key, whose own vault holds them (an NMI gateway's customer vault, a Stripe account), or the custodian that PSP names; or `platform`, the deployment's platform vault in the merchant's partition. Absent: each card is saved where the PSP that takes it keeps cards |
| `charge_psps` | the card PSPs, in order, that new card charges go to. Absent: every armed card PSP that can charge the vault's cards, the `card_vault` PSP alone when it is a PSP |

```yaml
settings:
  card_vault: mobius
  charge_psps: [mobius]
```

Every charge PSP must charge the vault's cards, which is known by rail and account:

- A card in a PSP's own vault charges only through that account. A Stripe customer
  belongs to its Stripe account; NMI Customer Vault gateway-issued tokens are
  valid only within their merchant account (`gateway_id`), so they charge only
  through PSPs on that same gateway account ([NMI Customer Vault FAQ](https://support.nmi.com/hc/en-gb/articles/33994528786705-Customer-Vault)).
- A custodian's card (Basis Theory, HyperSwitch) charges through any PSP naming that
  custodian.
- A `platform` card charges through any PSP whose rail accepts a proxied PAN or
  network token: NMI, and not `card_entry: server` (Stripe's own fields take the card).

A merchant whose settings break this is refused, for that merchant alone and never
fatally: over the API (`409 card_vault_unknown`, `charge_psp_unknown`,
`charge_psp_incompatible`), and a merchant document written into Vault directly is
not served (the previous one keeps serving). `platform` is also refused with:

- `409 platform_vault_unavailable` while the deployment configures no
  `card_vault` for its posture;
- `409 platform_vault_unreachable` while a charge PSP cannot charge the vault's
  cards. Creating such a PSP while `charge_psps` is absent is refused the same way.

Changing either setting moves no existing card. OpenRails-run renewals charge through
the PSP their subscription was sold through, which holds its recurring agreement.
`GET /v1/merchant/configuration` reports `platform_vault: {available}`.

Merchant Settings separates card custody from charge PSP selection. Configure a
merchant-owned Basis Theory account through `PUT /v1/merchant/custodians/{key}`
with its tenant, environment, public collection key and write-only private
credential; `GET /v1/merchant/custodians` returns configured flags, never secret
values. A credential must prove the exact vendor account before it can claim
that custody identity. `custodian` on PSP create/update references that merchant's
active vault in the same environment; an empty reference selects the PSP vault.
These settings do not turn on the separate SaaS shared-customer product.

An unused NMI or supported custodian card can be removed by its customer, or by
authorized merchant staff at
`DELETE /v1/admin/customers/{customer_id}/payment-methods/{id}`. Both paths use
the same account/alias/in-use guards and return success only after holder
confirmation. Existing financial instrument identity and transaction evidence
remain. A lost provider reply requires verification or an explicit retry, not a
fresh destructive operation.

The platform vault is operator configuration, not a merchant custodian:

```yaml
card_vault:
  kind: basis_theory
  tenant_id: <the vault tenant of this deployment's posture>
  kv_mount: billing-secrets
```

The deployment config is `card_vault` (`Config.CardVault` / `CardVaultConfig`
for embedded hosts). Standalone environment variables are `CARD_VAULT_KIND`,
`CARD_VAULT_TENANT_ID` and `CARD_VAULT_KV_MOUNT`.

When upgrading from `platform_vault`, rename that config section and the
`PLATFORM_VAULT_*` variables. Embedded hosts must rename the Go field and type.
The old config names are not aliases. Keep the existing credential document at
`platform_vault/<tenant_id>`; no credentials or cards need moving. The merchant
API's `platform_vault.available` response and `settings.card_vault` choice are
unchanged: the deployment section configures the operator's card storage,
while the merchant setting chooses whether to use it.

`card_vault.kv_mount` names the HashiCorp Vault KV v2 mount holding its keys,
at `<kv_mount>/<vault.scope_prefix>/platform_vault/<tenant_id>` (prefix default
`openrails`). It uses the existing `vault` connection and authentication.
Leave `vault.kv_mount` unset to keep merchant configuration in files. The
platform mount does not enable merchant configuration edits or move cards.
If the platform mount is omitted, it uses `vault.kv_mount` as before; at least
one must be set. An unreadable or missing document never causes a fallback
to another mount. When a merchant's `card_vault`
is `platform`, OpenRails records the merchant's identity in it once, as a
`billing.custodians` row it owns: kind and key `platform`, account
`<tenant_id>/<merchant_id>`, no credentials. Platform cards name that row as
their `custodian_id`, so the custodian checks hold for them as for any
custodian. The key `platform` is reserved: no merchant custodian takes it.

### Same-card recovery for imported NMI subscriptions

For an imported NMI schedule in `awaiting_method` whose current PSP-held card
has no recurring agreement (or an active agreement without its storing
transaction), the customer can confirm that exact card through
`PUT /v1/me/subscriptions/{id}/payment-method` with its `payment_method_id`.
On approval, that explicit selection becomes the subscription's own card,
including when it previously followed the customer's default. A later change
between following the default and owning a card invalidates an older confirmation.
This does not charge. One durable `nmi_recurring_confirmation` operation owns
the zero-value verification, even when clicks carry different request keys.
An unresolved confirmation returns `409 resource_conflict`; the subscription
stays held and its recovery view reports `payment_in_progress`.

A lost response is looked up by the persisted order reference on the same NMI
account. Empty, ambiguous or mismatched results never permit a resend. After
confirmation, the agreement, attributed history decision
`recurring_agreement_restored`, retry date and successful operation commit
together. The existing retry worker collects later, subject to normal write
modes. A definitive decline remains held and repeated clicks return that
refusal. Cancellation, a changed card/version or a changed unpaid period prevents
an old confirmation from restoring the agreement.

This repair does not reauthorize revoked or inactive consent. Staff selection,
following the default with `null`, engine subscriptions, platform-held cards
and ordinary card replacement keep their existing behavior.

### Platform NMI purchases and subscriptions

Platform collection supports new and saved cards on NMI orders, including subscriptions.
`OrderPayment` collects it with Basis Theory Web Elements, sends only the token
intent and billing details to OpenRails, and charges through the existing NMI
sale operation. A saved platform card can pay another one-time order through
an offered NMI account where it has active card-on-file consent and a storing
transaction reference. Order payment options include the eligible
`saved_payment_method_ids`; the pay request sends only `payment_method_id`.
Before submitting, the operation verifies the stored token's tenant and merchant
container and rechecks the card and its consent. Changing the card vault for new cards
does not move the saved card. Subscription purchases establish a separate
recurring agreement; card reuse consent alone does not authorize renewals.
The existing renewal worker charges the saved token without CVV and cites that
agreement's original NMI transaction. An uncertain result stays with its
accepted attempt until resolved.

Customers can replace a subscription's card with another saved platform card.
When needed, a customer initiated zero amount verification establishes its
recurring reference. Cancellation and resume use the existing subscription
lifecycle. Adding a new platform card outside checkout remains separate work.

Collection stays on the original NMI account until cross-account transaction
references are qualified. An unavailable account holds collection for the
operator; it does not invalidate the customer's card. Account switching,
migration, network tokens and Stripe platform collection remain unsupported.
Saving cards where the PSP that takes them keeps them remains the default.

The operator-managed KV document at the path above has this shape:

```json
{
  "api_key": "<private platform application key>",
  "account_updater": false,
  "account_updater_lookahead_days": 14,
  "collection_applications": {
    "<merchant UUID>": {
      "id": "<public collection application UUID>",
      "public_api_key": "<public collection application key>"
    }
  }
}
```

Provision a distinct public collection application for each merchant in the
configured tenant, with only token-intent creation permission and the merchant's
allowed browser origins. The private application needs token-intent reads, token reads,
token creation and the detokenizing proxy permissions. Do not grant token
reveal or proxy access to a browser application. Application provisioning and
permission verification are operator steps; OpenRails does not create them.
Missing credentials or a shared collection application fail closed. Only the
public key appears in customer configuration.

Saved platform card removal also needs `token:delete`. Before reading or deleting
the card, OpenRails uses `GET /applications/key` with the same private key and
checks the returned tenant against the accepted removal. A failed lookup or
different tenant keeps removal pending and the card blocked from further use;
a token's 404 alone is not proof of deletion. Verify this application lookup
when provisioning or rotating the key. No management key is stored in the engine.

Token intents have no merchant containers. Before charging, OpenRails checks
the vendor-reported `tenant_id` and `created_by` against this merchant's
configured application. After approval, conversion uses an operation-derived
idempotency key and the fixed container `/merchants/<merchant UUID>/`.
OpenRails verifies the returned tenant and container before storing an active
`custodian=platform` method. A decline does not convert or accept a method.
Existing card retention/consent policy still applies.

An unconfirmed browser request retries the same payment details and
idempotency key, including when paying with a saved card. An accepted sale's
durable submission fence prevents another charge; the sale's provider operation completes conversion from the retained receipt.
An expired token intent can leave a confirmed payment with no saved method,
following the existing Basis Theory recovery policy.

For a subscription order, a storage failure does not withhold the payment or
paid access. The subscription waits for a usable card while the existing sale
operation retries storage. Recovery cannot override a cancellation or a card
agreement selected or revoked since the purchase. If the token intent expires,
the paid subscription remains awaiting a payment method, under the existing
bounded dunning policy.

The hosted payment page permits `https://*.basistheory.com` in `script-src`,
`frame-src`, and `connect-src`. Embedded hosts must permit those sources too.
See the vendor's [token intents](https://developers.basistheory.com/docs/api/tokens/token-intents),
[token containers](https://developers.basistheory.com/docs/api/tokens), and
[Web Elements security guidance](https://developers.basistheory.com/docs/sdks/web/web-elements/v3/security).
Local provider fakes verify the request/receipt contract, not live vendor
qualification. Sandbox qualification of application permissions, iframe
collection, proxy approval/decline, and conversion recovery remains required.

`payment_methods.wallet_card_id` names the customer wallet card a merchant's
copy was made from; it is NULL for a card given to the merchant.

## Declaring a custodian

A custodian is an account the merchant holds with a third party, exactly like a
PSP is an account it holds with a gateway — so it is declared once, in its own
block, and REFERENCED by every PSP whose gateway charges the cards it holds:

```yaml
merchants:
  acme:
    psps:
      mobius-bt:
        rail: nmi
        gateway_id: "7654322"          # the NMI gateway id — the PSP still charges
        custodian: bt                  # ← the reference
        secrets:
          security_key: <NMI security key>
      mobius-bt-backup:
        rail: nmi
        gateway_id: "7654323"
        custodian: bt                  # same vault, second gateway
        secrets:
          security_key: <NMI security key>

    custodians:
      bt:
        kind: basis_theory
        account_id: <BT tenant id>     # the custodian-native tenant identity
        public_api_key: <BT public application key>   # payment-page config
        network_tokens: false
        account_updater: false                        # real-time Account Updater add-on
        account_updater_lookahead_days: 14            # optional; default 14
        secrets:
          api_key: <BT private application key>         # the only custodial secret
```

`custodians.<key>.<kind>` is to a custodian what `psps.<key>.<rail>` is to a
PSP. Everything a kind needs — its secret slots, its declared settings, which
rails can charge its instruments, how a browser tokenizes against it — is data
in the custodian registry (`internal/custodians`), so a second vendor
(HyperSwitch, JusPay, Spreedly) is a descriptor plus its implementation, not a
new branch in the validator, the reconciler, the browser projection and the
webhook router.

Consequences, all enforced rather than documented:

* Only rails the custodian's registry entry names may reference it — today
  `nmi`, the one rail with a detokenizing-proxy charge path. Any other rail
  refuses the push.
* A PSP referencing an undeclared custodian key fails the push, and the DB
  carries a composite foreign key so it cannot reference another merchant's.
* A custodial PSP must not also declare `tokenization_key` / `tokenization_url`:
  the browser tokenizes against the **custodian**, so the rail's own tokenizer
  key would be dead config on a purchase path.
* `GET /purchase/config` reports `custodian` alongside `rail` and serves the
  custodian's public key — a frontend drives whoever holds the card.
* Secrets are scoped by custodian IDENTITY, not by the merchant's nickname for
  it: `custodians/<kind>/<environment>/<account_id>/<key>`, the exact shape
  `psps/<rail>/<environment>/<account_id>/<key>` already has — and they are read
  through the same rotation version floor (`custodians.credential_versions`), so a
  rotated custodial key cuts over on every node at once.
* `account_updater` arms Basis Theory's **real-time Account Updater** for the
  Visa and Mastercard cards this vault holds. NMI charges the vault's own card
  number, so the vault must be asked whether a card was reissued; a PSP or
  HyperSwitch updates its cards in place. One card is refreshed at a time:
  * ahead of a renewal: a change to a subscription's period, status or card
    marks its card, and a pass every 6 hours queues each marked card whose
    next renewal (`active`, `past_due` or `awaiting_method`) is inside
    `account_updater_lookahead_days` (default 14) and that was not checked for
    it, spreading the requests over the following hour. A vault armed later is
    first asked about a card ahead of that card's next renewal;
  * after a renewal declined as an expired or reissued card. If the vault is
    not armed, finds no new card or refuses the request, the customer is asked
    for one. A declined card any other holder keeps asks the customer at once.

  Results are applied as card versions, through the same writer the
  custodian's webhooks use: a reissue (a new token, or a new expiry on the
  same token) updates the same payment method, clears any park and retries the
  subscriptions waiting on it; another brand holds its mandates for the
  customer's consent; a closed account closes it; a "contact cardholder" answer
  prompts the customer, or closes a Mastercard card. Nothing is ever deleted or
  canceled. Each request is a provider intent whose id is Basis Theory's
  idempotency key, and asks with token deduplication off, so a reissue never
  takes another stored card's token. Throttling, timeouts and server errors are
  retried on the intent's backoff and never mark a card checked. A refused key
  raises one `card.account_updater.unauthorized` finding and holds the vault's
  checks until it answers again or the finding is resolved; an answer that
  cannot be applied raises `card.account_updater.unapplied`.
  Both settings are off/default until declared — the updater is a priced
  add-on, and an unarmed vault's cards are never refreshed. The private
  application key needs `account-updater:real-time:invoke`. For the platform
  vault the operator sets `account_updater` and
  `account_updater_lookahead_days` in its credential document; a malformed
  value there leaves the updater unarmed and the vault otherwise working.
* The retired inline keys (`settings.custodian_account_id`,
  `custodian_public_api_key`, `custodian_network_tokens`, the
  `custodian_api_key` secret) and the retired `vaulted_card` keys
  (`gateway_account`, `nt_charges`) fail the push with a move/rename error.
  There are no aliases.

### Why custody is not a PSP setting

One vault routinely backs several gateways: a live acquirer and a sandbox, or
two acquirers fronting the same card file. Copied per PSP, the tenant id and the
application key drift, and the same custodian silently becomes two. Declaring it
once and referencing it states it once. A custodian is never a rail: the gateway
that charges a Basis-Theory-held card is still NMI, and `custodian` is the field
that says who holds it.

## Adding a custodian

Custody values are a closed set (`models.Custodians()`, mirrored by the
`payment_methods_custodian_check` constraint). Adding one is a migration plus a
constant — deliberately, so an unknown custodian cannot arrive silently on a
money path.

## Saving a HyperSwitch card

`POST /v1/me/payment-methods` names the NMI PSP whose configured custodian is
HyperSwitch. Send `psp_id`, optional `billing_details`, and a stable
`Idempotency-Key`; omit `token` and `card`. The response is the customer's
pending payment method, with `status: "requires_action"` and
`next_action.type: "capture"`.

The action's payload initializes the vendor-owned fields: `custodian_id`,
`session_id`, `customer_id`, `api_base_url`, `sdk_url`, `public_api_key`,
`sdk_authorization`, and `expires_at`. Keep this short-lived response private.
Card data goes to the custodian. After its fields return the session token,
confirm the same pending resource:

```http
POST /v1/me/payment-methods/{id}/confirm
Content-Type: application/json

{"capture":{"custodian_id":"…","session_id":"…","token":"…"}}
```

OpenRails verifies the token with that vendor session and customer before
attaching the permanent instrument. Retrying the original key returns the
accepted setup; retrying confirmation with its accepted token returns the
saved method. Another customer's method, a different token, or a changed
custodian account is refused. Saving creates no payment or mandate: a later
order charges the card. If the instrument is already saved for this customer,
confirmation returns that method and marks the pending resource as replaced.

The SDK authorization is encrypted at rest and erased on confirmation or
expiry. The order expiry job (every 5 minutes) removes a setup once its
session expires or 24 hours after it began. A captured card has
no holding PSP (`psp_id: null`); its orders and recurring agreements select the
processor that can reach its custodian.

Purchase creation no longer accepts a price or a mode. New payments are order
attempts. Prelaunch purchase history is not migrated or reconstructed.

## Choosing a card

A purchase or pay-now names its method (`payment_method_id` or a new card);
naming neither is `payment_method_required` (400). Renewals and automatic
usage order collection charge by a two-level default: a subscription's own card,
else the customer's default card for its currency
(`PUT /v1/me/default-payment-methods/{currency}`, listed in that card's
`default_currencies`), which also collects their usage orders there. A renewal
with neither, or whose card carries no active recurring agreement, waits for
the customer (`awaiting_method`); it never charges another card.

Provider mirror: none is needed. OpenRails names the exact instrument on
every charge (Stripe payment method id, NMI billing id), so Stripe's customer
default and NMI's vault priority never select a card for OpenRails-owned
billing. A provider-owned NMI schedule bills its vault's priority-1 entry;
every saved NMI card is a vault of its own.
