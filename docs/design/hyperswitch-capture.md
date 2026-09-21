# HyperSwitch card setup through checkout sessions

First bounded #297 adoption cut. The pinned vendor corrections, charge/invoice adapter, and engine-owned recurring scheduler are separate qualification steps. This cut prepares a vendor capture session and attaches only its completed, owned payment method. PAN/CVC travels from vendor-owned browser fields to the vendor, never through OpenRails.

## Shared Go/HTTP contract

Reuse CreateCheckoutSession, GetCheckoutSession and ConfirmCheckoutSession, and their existing `/v1/merchant/checkout-sessions` and `/v1/me/checkout` route families. Add `mode: "payment_method"`. It requires an explicit immutable PSP ID, an authenticated merchant-owned customer, and no price/subscription/payment token. The server resolves and freezes the PSP's immutable custodian. No new route family, table, purchase or ledger entry.

Public DTO changes:

- `CheckoutPayment.PSPID uuid.UUID` (`psp_id,omitzero`): required for setup, an assertion if supplied for a price-bearing checkout. Existing priced selection remains supported.
- `ConfirmPayment.Capture *CustodianCaptureReference` (`capture,omitempty`): `CustodianID uuid.UUID`, `SessionID string`, `Token string` (the vendor session-scoped token). These are assertions against the stored setup session and the vendor's completed associated payment method; a browser-provided ID alone is never ownership evidence.
- `CheckoutSession.Capture *CustodianCaptureAction`: immutable custodian ID/kind, vendor session/customer IDs, vendor API base/public key, short-lived vendor session secret, expiry. These are typed fields rather than arbitrary rail_data entries. They are owner-authorized only, no-store, excluded from caches/logs, and cleared at terminal completion while the nonsecret binding remains.
- `CheckoutSession.PaymentMethodID *PaymentMethodID`: the attached engine method after setup succeeds.
- `CheckoutSession.PriceID`, `Amount`, `Currency` become nullable pointers: setup has no monetary terms; price-bearing JSON retains its existing exact values. Consumers compiling against these Go fields must be migrated in the same cut. There is no fake zero-money purchase.

A later paid checkout references the already-owned local PaymentMethodID and performs its own authority, PSP/instrument, amount and agreement checks. It does not replay an arbitrary vendor capture reference as payment authority. The old internal `bt_token_intent_id` checkout field is removed in the eventual coordinated hard cut, not kept as an alias.

## Persistence and invariants

Existing checkout_sessions changes only:

1. Add `payment_method` to the mode CHECK.
2. Make price_id, amount and currency nullable.
3. A conditional CHECK requires all three NULL and payment_id/subscription_id NULL for setup; every existing mode still requires price_id/amount/currency NOT NULL. Existing merchant-scoped FKs remain.
4. Freeze merchant, engine customer, PSP, custodian, vendor merchant/customer/session and expiry in private rail_state. Generic progress cannot replace accepted bindings. Clear unnecessary short-lived secrets on terminal completion while retaining the binding and attached local method for replay.

The trusted HyperSwitch API base and SDK asset endpoint belong to host deployment configuration, not merchant-writable custodian settings. HTTPS is required outside an explicit local-test scope. Merchant custodian settings contain only exact vendor merchant account/profile/public key and the private API-key secret. A tenant cannot redirect server credentials to an arbitrary endpoint.

Use a domain-separated SHA256 UUID derived from merchant plus payer as HyperSwitch merchant_reference_id and its authenticated exact lookup endpoint to recover the vendor customer. Create conflicts must read back that exact reference; never select the first returned customer. Freeze the vendor session once the client sees it. An uncertain preparation must not silently retarget an already accepted engine session.

The new mode branches before normal catalog/price resolution. It must reject mixed setup/payment inputs and preserve every normal priced-checkout validation. A setup mutation holds the session row while inserting/reusing the one matching local instrument and marking succeeded. Retried confirm returns that same engine method without another attachment or vendor mutation. Vendor session creation failures/uncertain responses must not manufacture a completed engine session.

The default safe vendor metadata request is GET payment-methods with fetch_raw_detail=false and force_sync=false. The vendor organization also keeps raw-return permission disabled. Session completion readback must explicitly name the proposed session-scoped Token among associated methods. Resolve that token through the normal metadata endpoint to the permanent vendor payment-method ID; do not accept an arbitrary permanent ID in place of the session association. Merchant/customer identity, custodian credentials, storage type and masked card metadata must match; any raw_payment_method_data refuses. The actual browser proof shows no PAN crosses the vendor→OpenRails HTTP boundary, not merely that a decoder ignores it.

Core custody remains merchant-owned. SaaS may later implement consumer sharing with explicit cross-merchant grants; this setup flow does not imply such sharing.

## Qualification

The actual pinned local HyperSwitch/router/card-vault build and browser-driven vendor capture passed. The ordinary release workflow covers shared Client embedded/HTTP setup parity, correct attachment and exact replay; foreign merchant/customer/session/method rejection; incomplete/expired session refusal; malformed/raw metadata refusal; concurrent confirm; setup produces no payment/invoice/ledger/subscription; priced checkout still requires its price and amount. No live NMI/CCBill operations, vendor upstream publication or production deployment.

Subsequent cuts reuse charge.Charger, native currency/COF helpers, account-bound receipts and durable operation custody for HS charges/invoices. Current neutral-custody “renewal” coverage calls a Charger directly; it does not prove an engine-owned due-period scheduler. That scheduler requires an explicit later lifecycle design, coordinated with the #542 payment owner.

The custodian vocabulary/descriptor, trusted host URL configuration and token-only setup adapter are implemented. Existing Basis Theory checkout and invoice transport dispatch now explicitly refuse other custodian kinds until their qualified adapter exists, so adding the HyperSwitch descriptor cannot route its credentials through the Basis Theory or PSP-native transport. The existing global `(kind, environment, account_id)` custodian identity uniqueness remains intact; two core merchants cannot silently claim the same vendor account.

Before the later form adapter arms, the scoped client must GET the authenticated vendor `/v2/proxy` preflight and require the exact `openrails-nmi-form-v2` contract, strict policy,64KiB response bound and matching exact destination/POST/nmi_classic route. Stock/missing/disabled/mismatched deployments refuse before any payment dispatch. Generic health is not qualification. Deployment commit/image/config digests belong in operational proof, not an unverifiable manually supplied digest in the host API. This preflight is not full browser capture qualification.

Engine scheduling boundary: `RebillDriverOpenRails` currently selects OpenRails recovery of a provider-scheduled NMI subscription; it does not establish an engine-owned cadence. A later neutral recurring cut must put explicit scheduling/recovery authority on the subscription/obligation, reuse the durable operation ledger and accepted RenewalTerms/receipts, and must not send neutral renewals through `rebill_subscription`. Initial enrollment needs a qualified customer charge establishing a recurring agreement; unqualified zero-value/free-trial enrollment refuses. No scheduler changes belong in this capture cut.

Setup also requires the authenticated concrete patched deployment preflight before issuing or replaying browser SDK authority, because stock v2 lacks the qualified SDK resource-binding correction. Attachment rechecks and share-locks the current PSP/custodian account, environment and profile after metadata readback; archive or retarget before that short transaction refuses. Same-key changed email/name input conflicts, while GetSession recovers the accepted owned action without changing its original vendor identity.

Saved-card replacement through the PSP-native NMI path refuses neutral custody before enqueue/client resolution. Saved-card removal dispatches to the custody-specific durable handler described below. A terminal setup stores historical attached-method identity. Its replay never recreates a removed method, and a legitimately absent local method does not block archive/restore. Charge operations must still resolve a currently owned, usable method; the historical ID is not charging authority.

HyperSwitch v2 requires customer name/email while AuthKit native session tokens deliberately omit profile data. Setup accepts the existing payment.email/name_on_card fields as non-authoritative contact inputs when authenticated host contact metadata is absent, preferring host values when present. Effective values are validated and fingerprinted. The authenticated merchant/payer IDs alone select the vendor reference; reusing another person's email cannot select their customer or payment methods. No placeholder identity or profile database reach-in is used.

The qualified local router source is 77db63fd08b404dbcc6b62fbee25c754ebcb5963, based on upstream main329f7d7d. Its baked image is sha256:1a1c9cad6088c2da3cd3202d89d7c8616e22e68f279a4b080617e03ebe2481eb; the running binary is sha256:8ee4f12a797b3835f1622b8465d4f367598bcbe23825bd66b527d1df2b2c419d. SDK upstreambc46e0a5/shared-code1af3e5da requires the local v2 path/storage-consent fixes at5bfc28c89d and the served CSP qualified at a5b56cb. These are local corrected builds, not interchangeable stock vendor assets.

Actual Chromium coverage uses default volatile tokenization and explicit Save card. Only Save card attaches a persistent method, with zero financial rows. Both merchant-page and vendor-iframe responses serve restrictive CSP; optional wallet, Braintree, font and telemetry attempts are recorded and blocked, with zero external HTTP requests. Production must preserve equivalent CSP and disabled preload/logging settings on both documents. Exact PAN, merchant API-key, SDK-authorization and native-token log checks pass on the v2 image.

A deployment may be downgraded after setup preparation. Before a new attachment, confirm repeats the same authenticated concrete preflight; missing, v1 and disabled-policy responses refuse without a local method. A historical terminal replay remains local and does not require the vendor to be available or qualified.

The invoice extension uses the captured permanent card through the qualified v2
NMI form proxy. Save card remains nonfinancial. An explicit customer invoice
payment establishes an unscheduled agreement; later merchant invoice collection
uses that agreement. Recurring subscription enrollment is outside this cut.

`TestHyperSwitchInvoiceClientWorkflow` is the ordinary standalone HTTP Client
gate in `compatibility/workflows.tsv`. The separately selected
`TestHyperSwitchActualBrowserInvoice` (tags `integration browser hyperswitch`)
uses `OPENRAILS_HYPERSWITCH_FIXTURE` and the pinned local vendor SDK/router. It
covers default volatile refusal, explicit persistent Save card with no financial
effects, browser Pay invoice and same-key replay/conflict, exact USD/JPY amounts,
CIT/MIT agreement continuity, lost reply and a new-process verifier without
resubmission, decline, and terminal archive restore without a provider adapter.
This replaces the former capture-only manual selector; no external-vendor CI job
or live financial qualification is implied.

A fresh submission owner may finish a typed pre-dispatch refusal as not executed:
local validation, masked-method readback, or contract preflight failed before
entering the proxy POST. This permits a new payment key after repair. Existing
submission markers and every uncertain error after POST entry remain verify-only;
a proxy HTTP error never proves that the PSP did not execute.

A lost claim before the submission fence resumes execution; claim count is not
proof of submission. Executors reload canonical progress, and parking, expiry and
nonexecution completion check stored fence state. A fresh pre-dispatch refusal
uses a private owner capability and the existing sealed-evidence writer, bound
to the accepted payload and fence. Generic progress fields cannot create that
proof. It survives a failed local commit; terminal completion retains the usual
invoice result. A later verifier may finish that result without another charge.

### Customer-requested stored-card deletion

The existing authenticated payment-method DELETE supports native NMI and qualified HyperSwitch native custody. The operation freezes the owned method and rejects live subscription use or unresolved operations. Acceptance parks the method with its durable operation ID, so a subsequent charge cannot start. HyperSwitch serializes all local aliases of a vendor handle with capture/import/remap: an unused alias detaches locally, and only the final reference requests physical erasure. Pending detach decisions participate in that same exclusion. Completed physical deletion remains a handle tombstone in the existing operation history, so a stale vendor read cannot recreate an alias after deletion commits. Completed local-only detachment leaves the surviving card available.

Verified self-service deletion works with the default maintenance/destructive-convergence switch off. This exception requires canonical admission from the authenticated payer and retained matching actor/target attribution; an `origin=user` label alone grants nothing. Operator, system and unattributed commands retain the maintenance gate. Provider read-only mode, ownership checks, unresolved-operation fences, and existing destructive rate and volume ceilings still apply to customer deletion. There is no new public setting.

An uncertain delete returns HTTP202 and retains its fence. HyperSwitch recovery retries only the exact native DELETE authorized by the pinned vendor contract;404, redacted metadata or lookup absence is not physical-erasure evidence. Terminal replay does not delete a newly captured method. Archive history preserves the original decision without restoring a deleted local method.

Native NMI deletion also freezes whether it deletes one billing entry or the whole vault. Customer/account/vault admission locks serialize local create, declared import, remap and deletion; no transaction lock crosses HTTP. NMI refuses deletion of the final billing address, so whole-vault deletion additionally requires a fresh provider read containing exactly the accepted sole billing entry. Provider-only sibling entries cause refusal. There is no fallback from an uncertain entry deletion to whole-vault deletion. Both custody paths remove the fenced local method and commit their retained terminal receipt in one transaction; rollback leaves the row available to a fresh recovery process. Completed whole-vault and exact-entry decisions block stale reattachment and archive resurrection.
