# HyperSwitch card setup through checkout sessions

First bounded #297 adoption cut. The pinned vendor corrections, charge/invoice adapter, and engine-owned recurring scheduler are separate qualification steps. This cut will prepare a vendor capture session and attach only its completed, owned payment method. PAN/CVC travels from vendor-owned browser fields to the vendor, never through OpenRails.

## Proposed shared Go/HTTP contract

Reuse CreateCheckoutSession, GetCheckoutSession and ConfirmCheckoutSession, and their existing `/v1/merchant/checkout-sessions` and `/v1/me/checkout` route families. Add `mode: "payment_method"`. It requires an explicit immutable PSP ID, an authenticated merchant-owned customer, and no price/subscription/payment token. The server resolves and freezes the PSP's immutable custodian. No new route family, table, purchase or ledger entry.

Proposed DTO delta, for root review before implementation:

- `CheckoutPayment.PSPID uuid.UUID` (`psp_id,omitzero`): required for setup, an assertion if supplied for a price-bearing checkout. Existing priced selection remains supported.
- `ConfirmPayment.Capture *CustodianCaptureReference` (`capture,omitempty`): `CustodianID uuid.UUID`, `SessionID string`, `PaymentMethodID string`. These are assertions against the stored setup session and the vendor's completed associated payment method; a browser-provided ID alone is never ownership evidence.
- `CheckoutSession.Capture *CustodianCaptureAction`: immutable custodian ID/kind, vendor session/customer IDs, vendor API base/public key, short-lived vendor session secret, expiry. These are typed fields rather than arbitrary rail_data entries.
- `CheckoutSession.PaymentMethodID *PaymentMethodID`: the attached engine method after setup succeeds.
- `CheckoutSession.PriceID`, `Amount`, `Currency` become nullable pointers: setup has no monetary terms; price-bearing JSON retains its existing exact values. Consumers compiling against these Go fields must be migrated in the same cut. There is no fake zero-money purchase.

A later paid checkout references the already-owned local PaymentMethodID and performs its own authority, PSP/instrument, amount and agreement checks. It does not replay an arbitrary vendor capture reference as payment authority. The old internal `bt_token_intent_id` checkout field is removed in the eventual coordinated hard cut, not kept as an alias.

## Persistence and invariants

Existing checkout_sessions changes only:

1. Add `payment_method` to the mode CHECK.
2. Make price_id, amount and currency nullable.
3. A conditional CHECK requires all three NULL and payment_id/subscription_id NULL for setup; every existing mode still requires price_id/amount/currency NOT NULL. Existing merchant-scoped FKs remain.
4. Freeze merchant, engine customer, PSP, custodian, vendor merchant/customer/session and expiry in private rail_state. Generic progress cannot replace accepted bindings. Clear unnecessary short-lived secrets on terminal completion while retaining the binding and attached local method for replay.

The new mode branches before normal catalog/price resolution. It must reject mixed setup/payment inputs and preserve every normal priced-checkout validation. A setup mutation holds the session row while inserting/reusing the one matching local instrument and marking succeeded. Retried confirm returns that same engine method without another attachment or vendor mutation. Vendor session creation failures/uncertain responses must not manufacture a completed engine session.

The default safe vendor metadata request is GET payment-methods with fetch_raw_detail=false and force_sync=false. The vendor organization also keeps raw-return permission disabled. Session completion readback must explicitly name the proposed payment method among associated methods. Merchant/customer identity, custodian credentials, storage type and masked card metadata must match; any raw_payment_method_data refuses. A local proof must show no PAN crosses the vendor→OpenRails HTTP boundary, not merely that a decoder ignores it.

Core custody remains merchant-owned. SaaS may later implement consumer sharing with explicit cross-merchant grants; this setup flow does not imply such sharing.

## Required proofs

Use the actual pinned local HyperSwitch/router/card-vault build and a browser-driven vendor capture session. Exercise shared Client embedded/HTTP setup parity, correct attachment and exact replay; foreign merchant/customer/session/method rejection; incomplete/expired session refusal; malformed/raw metadata refusal; concurrent confirm; setup produces no payment/invoice/ledger/subscription; priced checkout still requires its price and amount. No live NMI/CCBill operations, vendor upstream publication or production deployment.

Subsequent cuts reuse charge.Charger, native currency/COF helpers, account-bound receipts and durable operation custody for HS charges/invoices. Current neutral-custody “renewal” coverage calls a Charger directly; it does not prove an engine-owned due-period scheduler. That scheduler requires an explicit later lifecycle design, coordinated with the #542 payment owner.
