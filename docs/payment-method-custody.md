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
| `basis_theory` | `nmi` | BT card-token id (`rail_method_ref`) | yes |
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

## Where new cards go: `settings.card_custody`

A merchant setting chooses who holds the cards it saves from now on:

| `card_custody` | New cards are held by |
|---|---|
| `merchant` (or absent) | the merchant's own vault: its PSP, or the custodian a PSP names |
| `platform` | the deployment's platform vault, in the merchant's partition; the merchant's own PSP accounts still charge them |
| `hosted` | reserved for a private vault the platform hosts: `400 card_custody_reserved` |

Changing it moves no existing card. `platform` is refused with:

- `409 platform_vault_unavailable` while the deployment configures no
  `platform_vault` for its posture;
- `409 platform_vault_unreachable` while a live card PSP cannot charge the
  vault's cards: Stripe (its own fields take the card) or `card_entry: server`.
  Archive that PSP, or keep `merchant`. Creating such a PSP for a `platform`
  merchant is refused the same way.

A merchant or PSP document written into Vault directly that breaks these rules
is not served. `GET /v1/merchant/configuration` reports `platform_vault:
{available}`.

The platform vault is operator configuration, not a merchant custodian:

```yaml
platform_vault:
  kind: basis_theory
  tenant_id: <the vault tenant of this deployment's posture>
```

It needs `vault.kv_mount`: its keys are at
`<kv_mount>/<scope_prefix>/platform_vault/<tenant_id>`. When a merchant chooses
`platform`, OpenRails records the merchant's identity in it once, as a
`billing.custodians` row it owns: kind and key `platform`, account
`<tenant_id>/<merchant_id>`, no credentials. Platform cards name that row as
their `custodian_id`, so the custodian checks hold for them as for any
custodian. The key `platform` is reserved: no merchant custodian takes it.

### Platform NMI purchases and subscriptions

Platform collection supports new and saved cards on NMI orders, including subscriptions.
`OrderPayment` collects it with Basis Theory Web Elements, sends only the token
intent and billing details to OpenRails, and charges through the existing NMI
sale operation. A saved platform card can pay another one-time order through
an offered NMI account where it has active card-on-file consent and a storing
transaction reference. Order payment options include the eligible
`saved_payment_method_ids`; the pay request sends only `payment_method_id`.
Before submitting, the operation verifies the stored token's tenant and merchant
container and rechecks the card and its consent. Changing custody for new cards
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
Existing merchant custody remains the default.

The operator-managed KV document at the path above has this shape:

```json
{
  "api_key": "<private platform application key>",
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

Token intents have no merchant containers. Before charging, OpenRails checks
the vendor-reported `tenant_id` and `created_by` against this merchant's
configured application. After approval, conversion uses an operation-derived
idempotency key and the fixed container `/merchants/<merchant UUID>/`.
OpenRails verifies the returned tenant and container before storing an active
`custodian=platform` method. A decline does not convert or accept a method.
Existing card retention/consent policy still applies.

An unconfirmed browser request retries the same payment details and
idempotency key, including when paying with a saved card. An accepted sale's
durable submission fence prevents another charge; the existing verifier completes conversion from the retained receipt.
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
        account_id: "7654322"          # the NMI gateway id — the PSP still charges
        custodian: bt                  # ← the reference
        secrets:
          security_key: <NMI security key>
      mobius-bt-backup:
        rail: nmi
        account_id: "7654323"
        custodian: bt                  # same vault, second gateway
        secrets:
          security_key: <NMI security key>

    custodians:
      bt:
        kind: basis_theory
        account_id: <BT tenant id>     # the custodian-native tenant identity
        settings:
          public_api_key: <BT public application key>   # payment-page config
          network_tokens: false
          account_updater: false                        # batch account updater add-on
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
* `account_updater` arms the **batch account updater**: a periodic worker that,
  ahead of each renewal, asks the custodian to refresh the cards backing
  subscriptions due inside `account_updater_lookahead_days` (default 14, and
  also how long a refresh stays fresh — a card is looked up once per cycle).
  Results are applied as card versions, through the same writer the
  custodian's webhooks use: a reissue gives the same payment method the new
  token and card and clears any park; another brand holds its mandates for the
  customer's consent; a closed account closes it; a "contact cardholder" answer
  prompts the customer, or closes a Mastercard card. Nothing is ever deleted or
  canceled.
  Both settings are off/default until declared — the updater is a priced add-on,
  and an unarmed custodian is never enumerated by the worker at all.
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
expiry. The normal cleanup sweep removes expired setups. A captured card has
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
