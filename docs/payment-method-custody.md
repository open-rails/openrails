# Payment-method custody

A stored card has two independent owners, and OpenRails records them separately.

| Axis | Question | Where it lives |
|---|---|---|
| **Processor** | Who *charges* the card? | `payment_methods.rail`; `payment_methods.psp_id` when the PSP holds it |
| **Custodian** | Who *holds* the card? | `payment_methods.custodian` |

They are orthogonal. The card can sit inside the processor that charges it
(Stripe, an NMI gateway vault) or in a neutral third-party vault that proxies
the PAN to whichever processor you point it at (Basis Theory today;
HyperSwitch / JusPay / Spreedly are the same shape). Custody is not a property
of the gateway, so it does not belong in the rail value.

`custodian` is never empty. "Stored at the processor itself" is the stated
value `psp`, not an absence — a DB CHECK enforces the vocabulary.

`psp_id` is set exactly when `custodian = 'psp'` (CHECK
`payment_methods_psp_custody_check`). A card a third-party custodian holds belongs
to no PSP: each charge routes to the one live PSP of the card's rail that
reaches the custodian (`charge.RoutePSP`); none, or two, is `ErrNoRoute`. An
obligation that names its PSP (a subscription, a frozen checkout instrument)
charges through that PSP, provided it still reaches the custodian.

## The matrix

| Custodian | Processor (rail) | Instrument handle | Real today |
|---|---|---|---|
| `psp` | `stripe` | `pm_…` on a Stripe Customer (`rail_method_ref`) | yes |
| `psp` | `nmi` | `customer_vault_id` (`rail_customer_ref`) | yes |
| `basis_theory` | `nmi` | BT card-token id (`rail_method_ref`) | yes |
| `basis_theory` | `stripe` | BT token proxied to Stripe | not built |
| third-party (HyperSwitch, JusPay, Spreedly) | any | provider token | not built |
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
          public_api_key: <BT public application key>   # checkout-page config
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
  key would be dead config on a checkout path.
* `GET /checkout/config` reports `custodian` alongside `rail` and serves the
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

## Choosing a card

A checkout or pay-now names its method (`payment_method_id` or a new card);
naming neither is `payment_method_required` (400). Renewals and automatic
invoice collection charge by a two-level default: a subscription's own card,
else the customer's default card for its currency
(`PUT /v1/me/default-payment-methods/{currency}`, listed in that card's
`default_currencies`), which also collects their invoices there. A renewal
with neither, or whose card carries no active recurring agreement, waits for
the customer (`awaiting_method`); it never charges another card.

Provider mirror: none is needed. OpenRails names the exact instrument on
every charge (Stripe payment method id, NMI billing id), so Stripe's customer
default and NMI's vault priority never select a card for OpenRails-owned
billing. A provider-owned NMI schedule bills its vault's priority-1 entry; an
in-place replacement removes the old entry, leaving the new card as the
vault's only card.
