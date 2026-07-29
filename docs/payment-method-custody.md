# Payment-method custody

A stored card has two independent owners, and OpenRails records them separately.

| Axis | Question | Where it lives |
|---|---|---|
| **Processor** | Who *charges* the card? | `payment_methods.rail` + `payment_methods.psp_id` |
| **Custodian** | Who *holds* the card? | `payment_methods.custodian` |

They are orthogonal. The card can sit inside the processor that charges it
(Stripe, an NMI gateway vault) or in a neutral third-party vault that proxies
the PAN to whichever processor you point it at (Basis Theory today;
HyperSwitch / JusPay / Spreedly are the same shape). Custody is not a property
of the gateway, so it does not belong in the rail value.

`custodian` is never empty. "Stored at the processor itself" is the stated
value `psp`, not an absence — a DB CHECK enforces the vocabulary.

## The matrix

| Custodian | Processor (rail) | Instrument handle | Real today |
|---|---|---|---|
| `psp` | `stripe` | `pm_…` on a Stripe Customer (`rail_method_ref`) | yes |
| `psp` | `nmi` | `customer_vault_id` (`rail_customer_ref`) | yes |
| `basis_theory` | `nmi` | BT card-token id (`rail_method_ref`) | yes — currently carried as `rail='vaulted_card'` |
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
(`pan_via_vault` / `network_token` / `provider_vault`).

## Known wrinkle: `rail = 'vaulted_card'`

A Basis-Theory-held card currently also carries `rail = 'vaulted_card'`, even
though the gateway charging it is NMI. That value is a second, weaker encoding
of `custodian = 'basis_theory'` and every rail-dispatching switch has to alias
it back to NMI. Retiring it — `rail` becomes `nmi`, custody carries the rest —
is tracked separately; `custodian` is already the field to read.

## Adding a custodian

Custody values are a closed set (`models.Custodians()`, mirrored by the
`payment_methods_custodian_check` constraint). Adding one is a migration plus a
constant — deliberately, so an unknown custodian cannot arrive silently on a
money path.
