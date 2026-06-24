# Billing vocabulary: rail vs provider vs integration

Three terms that are easy to conflate. They sit at different levels; keep them distinct.

## Rail
A payment **channel / method family** — the lane money moves through:
cards-via-NMI (`mobius`), cards-via-Stripe (`stripe`), hosted-via-CCBill (`ccbill`),
crypto-via-Solana (`solana`), and (planned, #581) cards-via-Authorize.Net.

In code this is the `models.Rail` type and the `rail` column; the enum **values**
(`mobius`, `stripe`, `ccbill`, `solana`, `paypal`, `admin`, `manual`) *are* the rails.
The client code for each rail lives in `internal/integrations/<rail>` (its **integration**).

> Renamed from "processor" in #582. The enum string values were left unchanged
> (`mobius` is still the NMI rail's id — a separate rename, if ever, is its own decision).

## Provider account
A specific **credentialed account on a rail** — one NMI MID, one Stripe account.
Modeled by `provider_accounts` / `provider_account_id`. **One rail can have many
provider accounts.** This is deliberately *not* called a "rail": a rail is the lane,
a provider account is an account on the lane. The reconciliation taxonomy's
`pull.*` plane ("provider-observed truth") is about a provider *account's* observed
facts, so "provider" is correct there.

## Integration
The Go client code that speaks a rail's external API, under
`internal/integrations/{nmi,stripeapi,ccbill,solana}`.

## A preserved third meaning: NMI's *acquirer* "processor"
NMI's webhook payloads expose **NMI's own** notion of a "processor" — the backend
acquiring processor behind the gateway — via wire fields like `processor_id` and
`processor_response_text`, plus decline strings such as
`transaction_was_declined_by_processor`. These keep the `processor` name because
they mirror **NMI's external wire format** (we don't own it). They are a different
concept from our rail and must not be renamed. See `internal/modules/webhooks/types.go`
(`NMIRailRef` carries `json:"processor"`).
