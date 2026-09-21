# Native NMI engine recurring transport

The native engine path uses the existing customer vault and direct sale API. The initial customer charge sends recurring credential-on-file indicators; each subsequent merchant charge sends the original recurring transaction reference. Neither call creates, rebills, updates or deletes an NMI subscription. Existing provider-owned subscriptions retain their separate native schedule path, including when a card is shared with an engine agreement.

`nmidirect.Charger.ChargeInitialRecurring` and `ChargeRecurringMIT` implement transport legs for the shared accepted-operation handlers. They do not establish customer consent, create local agreements, schedule jobs, retry requests or complete payment/access records. The handler must freeze the accepted account, vault, explicit billing entry, amount, currency, terms and recurring anchor; retain a submission fence before a financial send; and recover an unknown result through receipt reads only.

The supported native instrument is an authenticated, account-scoped single-card vault with an explicit matching billing identifier. A missing/default billing identifier, multiple billing entries, changed credentials or unreadable binding refuses before a financial send. Trials, zero-value setup, finite schedules and authentication requirements are admitted only when the shared enrollment contract supports them; this transport does not convert unsupported terms into ordinary paid enrollment.

Recurring receipt reads use the immutable account credentials to join an exact order search, exact approved sale and the vault's sole billing entry. `SaleEvidence.VaultBillingID` is explicitly the result of the vault read. It is not a claimed transaction response field. The current documented NMI transaction response does not establish a credential-on-file echo or transaction billing-entry identity; tests must not invent such fields. Single-card readback plus exact vault identity provides the instrument restriction implemented here. Provider/account qualification must establish that the submitted recurring contract is honored. Readback absence, multiple successful order matches, identity changes and contradictory outcomes remain unresolved and never authorize a second sale.

Only a parsed `response=2` with matching 2xx response code is a definitive recurring refusal. Free response text is discarded; the typed refusal retains only the minimal response/code fields required by the shared refusal writer. Communication errors, duplicate detection, malformed replies and missing transaction identifiers remain unknown. Pre-send validation/read failures alone carry `charge.ErrNotDispatched`.

## Qualification

Deterministic local HTTP-provider tests cover initial/anchored CIT and MIT fields, exact account/vault/billing/amount/currency/order, no native scheduling in the engine path, legacy provider scheduling on a shared card, refusal versus unknown results, lost responses, delayed receipt visibility and contradictory receipt identities. These fixtures are not merchant-account or production qualification.

Activation still requires the shared public-enrollment/due-worker/receipt integration tests and separately scoped provider account evidence, including applicable consent/authentication requirements. No real provider financial request or existing-customer migration is performed by these tests.

Sources checked 2026-09-21: [NMI Credential on File](https://docs.nmi.com/docs/credential-on-file), [vault transaction lifecycle](https://docs.nmi.com/docs/full-transaction-lifecycle-example), and [Query API](https://docs.nmi.com/reference/query).
