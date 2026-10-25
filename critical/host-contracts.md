# Critical host and checkout contracts

These 21 scenarios exercise public Go clients, real HTTP routes, the standalone
server, real PostgreSQL/AuthKit, and the shipped React component in Chromium.
The external Stripe protocol fixture keeps an independent economic journal.
PSP credential/routing coverage (J06) is maintained in the separate PSP suite.

| Journey | Scenarios | Primary evidence |
| --- | ---: | --- |
| J03 hosted checkout | 5 | Order capability scope and expiry, origin/frame limits, actual assets and configuration, built iframe completion binding |
| J07 customer directory | 4 | Issuer-bound SCIM CRUD/bulk, atomic invalid patch, cross-issuer isolation, revoked keys and copied/live directory profiles |
| J31 billing archive | 4 | Same-identity atomic restore, corruption/occupied-target refusal, restored write custody and explicit arming, unresolved-operation refusal |
| J32 merchant host | 4 | Provision/find/rename, racing slug ownership, deletion/restore, exact unused-group retirement and name reuse |
| J34 destructive controls | 4 | Actor ceiling and durable incident, unauthorized clear refusal, reviewed clear and effect replay, finding resolution cannot bypass the hold |

Archives preserve financial records, not deployment event-log streams. A source
cursor must fail explicitly at the destination and the consumer resynchronizes.
An unresolved admission prevents export; a failed export restores the original
write posture. A successful restore requires an operator to arm the destination
before provider writes resume. File-owned configuration is changed through a
real engine restart with a new declaration, rather than the Vault-only mutation
API.

`critical:build` builds the published SDK and both browser consumers. The main
Go suite owns browser execution; `playwright.critical.config.ts` selects the SDK
client spec and `playwright.hosted.config.ts` selects the hosted component spec.
Neither configuration discovers the other's tests. Required dependencies include
`OPENRAILS_CRITICAL_PAYMENT_ASSETS`, a built standalone fixture and CLI (the
`OPENRAILS_CRITICAL_SERVER_BINARY`/`OPENRAILS_CRITICAL_CLI_BINARY` prebuilds avoid
repeated compilation), PostgreSQL and Redis. Missing dependencies fail tests.

The completion-message regression is deliberately independent of payment
settlement: a legitimate frame must name the order in its pay URL, and switching
to another order on the same origin resets completion tracking. UI messages
never settle either order or produce provider money effects.
