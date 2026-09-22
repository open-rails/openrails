# Reviewed corrective composition

Current master50f24/v0.147 absorbed the billing candidate while several approved corrections remained in donor branches. This branch composes those exact inputs, preserving current-master behavior.

- #597: physical billing/worker DB identity and explicit River schema, published AuthKitv0.114.0/RiverKitv0.2.0. Same-name other-server, cancellation, one-connection and actual worker proofs pass.
- #598: atomic canonical transition/wakeup and true top-level successor commit acknowledgement; no savepoint/handler false acknowledgement, no unbounded active-job growth, original job retained on INSERT/COMMIT failure. Independent review approved fa1c312, including exact-owned fixture cleanup.
- #601: an obsolete operation wake completes only on definitive missing canonical ledger after an authorized purge. Transient errors and missing related rows retain accepted work.
- #591: port983 explicit accepted CheckoutSessionID through7c8275; opaque idempotency strings no longer determine session authority. Native archive, direct-key and actual self-HTTP regressions pass. Preserve master's unified Stripe fixture and its qualified decline behavior.
- #596: recursive client_secret removal before Stripe completed replay persistence, including SetupIntent/expanded/previous attributes. Exact JSON numbers and original-byte signature verification remain.
- #595: metadata-based package partition and E2E race coverage; local lifecycle copy fixes the concurrency bug this exposed. Original246 exact CI35680675280 passed all three jobs. Current-master63f084 source is identical for the CI/clock change and its contract check passes. Current-base census removes1,094 repeated roots; no physical test-source reduction or wall-time speedup is claimed.

Merge resolutions: retain current signup's special decline behavior; add the full596 redaction table; keep the intentionally deleted standalone Stripe fixture and update its unified replacement. No compatibility fallback, money/API assertion weakening, schema table or second queue was added.

Root separately composed e483+597+598 and passed all-repository build/vet plus actual worker/schema/recovery race controls (embed4.098s, River24.149s, zero unexpected HTTP). Source-specific proofs are in the original donor PR comments and reports. Final exact combined-head CI is required; prior donor or parent CI alone is insufficient. Live provider acceptance, consumer final pins and deployment remain distinct.


Final SQL gate composition: #602's e731d7939 checkout-session lock uses named SQLC with unchanged FOR SHARE/merchant predicates. #60498a1ac246 routes the three catalog/physical binding statements through gen.New on their original actual transaction/pools; fresh generation/vet/query audit and physical/role/cancel/worker controls pass. The two temporary file-wide infrastructure exceptions from602 are removed. Final sql-lint is clean with the original40 reviewed exemptions; final contract test passes5.146s.

Taking602 also preserves its stronger kill-switch assertion that a successful worker return has a committed successor at the ledger deadline, and removes the redundant top-level PI redaction block left by a historical merge. Root's combined real Stripe signup/signed webhook/replay tests pass11.314s, including nested/previous-attribute/array/SetupIntent redaction cases (zero unexpected egress). Fresh SQLC and original generated output match; final exact-head CI remains the merge gate.
