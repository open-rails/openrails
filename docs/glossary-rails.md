# Billing vocabulary: rail vs provider account vs channel vs integration

Terms that are easy to conflate. They sit at different levels; keep them distinct.

## Rail
A payment **gateway** you code against — the integration OpenRails speaks to move
money: NMI (`nmi`), Stripe (`stripe`), CCBill (`ccbill`), Solana (`solana`),
PayPal (`paypal`), and (planned, #581) Authorize.Net.

In code this is the `models.Rail` type and the `rail` column; the values
(`nmi`, `stripe`, `ccbill`, `solana`, `paypal`) *are* the rails. One adapter per
rail lives in `internal/integrations/<rail>` (its **integration**).

> Renamed from "processor" in #582. In #630 the white-label NMI rail id `mobius`
> collapsed into the gateway rail `nmi`: `mobius` is now a *provider-account name*
> on rail `nmi`, not a rail. There is no `rail_type` enum any more — `rail` is plain
> text so it can also hold an off-rail **channel** value (below).

## Provider account
A specific **credentialed account on a rail** — one NMI MID (e.g. the account
named `mobius` or `paykings`), one Stripe account. Modeled by `provider_accounts`
/ `provider_account_id`, and by `config.ProviderAccountConfig` / `ProviderAccountSet`
(keyed by account name, each carrying its `Rail` + credentials). **One rail can have
1..N provider accounts.** A rail is the lane; a provider account is an account on
the lane. The reconciliation taxonomy's `pull.*` plane ("provider-observed truth")
is about a provider *account's* observed facts, so "provider" is correct there.

## Channel (off-rail source)
An off-rail mechanism for **recording** a payment that never flowed through a
gateway: `admin` comps and `manual` entries (cash, bank transfer). Modeled by
`models.Channel` (`admin`, `manual`). A channel is **not** a rail — no adapter, no
credentials, no provider account. Off-rail payments are recorded in the same
source column as the rail (`payments.rail`), so a value there is either a `Rail`
or a `Channel`; the two Go enums keep the senses distinct.

## Integration
The Go client code that speaks a rail's external API, under
`internal/integrations/{nmi,stripeapi,ccbill,solana}`.

## A preserved third meaning: NMI's *acquirer* "processor"
NMI's webhook payloads expose **NMI's own** notion of a "processor" — the backend
acquiring processor behind the gateway — via wire fields like `processor_id` and
`processor_response_text`, plus decline strings such as
`transaction_was_declined_by_processor`. These keep the `processor` name because
they mirror **NMI's external wire format** (we don't own it). They are a different
concept from our rail and must not be renamed.

## Rail arming & resolution — one seam

A rail is usable for a merchant iff that merchant has an active provider
account on it, resolved **per-merchant at request time** via
`Runtime.Merchants.ActiveRailMerchantAccountScope` (rows in
`rail_merchant_accounts`; secret values through the store interface, which
serves manifest memory in MODE 1 and the Vault/DB secret store in MODE 2).
Every consumer — checkout gating, webhook client construction, provider pulls,
rebill charging — must resolve through this seam.

Two orthogonal axes, easy to conflate:

- **MODE 1 vs MODE 2** (`merchant_source: manifest` / `api`, #723/#724) is
  *who owns rail config*: in MODE 1 the operator owns all data and secrets and
  is the merchant (manifest-is-truth); MODE 2 is API-driven for true
  multi-tenant untrusted boundaries (openrails-saas). See
  `self-hosting-mode1.md`.
- **Embedded vs standalone** is only *how the process/routes are hosted*.
  Both modes run in either shape.

`Runtime.Rails` (the boot-time `embedded.Options.PaymentProviders` bridge) is
**not** a resolution source — it is empty on manifest hosts. Gating rail
availability on it is the #775 bug class (checkout fixed there; don't
reintroduce it elsewhere).
