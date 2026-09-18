# Compact workflow suite (#1013)

This is the coverage plan committed before broad test deletion. Production behavior,
public API, HTTP contract and database schema remain unchanged. CI workflow/script
changes and frontend consolidation belong to separate branches.

## Retained journey families

| Family | Production boundary and observations |
| --- | --- |
| Commerce | Catalog through Client, CCBill checkout replay, application program across four deployments, cancellation/resumption, durable refund, custodian charge |
| Spend and metering | Merchant/customer budget delegation, admission/capture/release across Client transports, usage-only payer through the real invoice sweep, remittance and provider obligations |
| Authority and browser | One subject under two merchant accounts, real AuthKit issuer and DPoP/mTLS, owner/automation/customer separation, CORS and body limits, fail-closed RLS and archive authorization |
| Provider uncertainty | NMI and Stripe lost replies, exact receipts, immutable instrument/amount/account binding, same-key body conflicts, request cancellation, operator resolution, process/runtime restart |
| Operator mechanisms | Hosted control plane, provider-account archive, custody boundary, account updater, maintenance record, monotonic health/cursor CAS, host-event acknowledgment and replay |
| Runtime and archive | One unchanged billing application on embedded/remote/SaaS transports, host transaction atomicity, manifest authority, ordinary purchases/subscriptions exported and restored, unsafe/busy archive refusal, actual archive CLI |

The existing six release-matrix scenario names and TSV schema stay unchanged.
The injected-only invoice uncertainty row is redundant with the real loopback-NMI
workflow; the large older conformance script is redundant with the unchanged
billing program and targeted transport workflows. Other matrix paths remain.

Small direct tests remain where end-to-end scenarios cannot enumerate arithmetic
bounds, signature preservation, SQL races or adversarial provider receipts. The
known scheduled-rebill exact-receipt gap remains open in #990; existing finalization
invariants are retained, and paused branches/reproductions are untouched. Live NMI
qualification is manual-tagged, never silently skipped by the ordinary CI suite.

## Deletion rule and limits

Retain the workflow/regression roots below and their declaration-level helper
closure. Remove other Go test declarations/files, including redundant mocks,
implementation-detail assertions and older fixture-only smoke tests. This is a
smaller owned acceptance suite, not a claim that every deleted assertion is
individually duplicated. No hidden historical test tree or default-skipped copy is
retained to make the count look smaller. Operator/provider qualification still
requires its stated external environment.

Shared infrastructure (`internal/integrationharness` and `internal/dbtest`),
provider fixtures, browser tests and the full generated contract snapshot all
count toward the final maintenance total. The first mechanically sound cut is
followed by fixture consolidation; the line budget is not met by minification.

## Initial retained declarations

These counts precede unused-import cleanup and further consolidation. Helpers are
kept only if reachable from retained tests; production files are not rewritten.

| File | Reason | Planned lines |
| --- | --- | ---: |
| `cmd/openrails/billing_archive_integration_test.go` | parity-archive | 118 |
| `cmd/openrails/runserver_mode1_integration_test.go` | shared helper | 36 |
| `embed/catalog_client_integration_test.go` | commerce | 85 |
| `embed/catalog_list_filter_integration_test.go` | commerce | 87 |
| `embed/commerce_client_integration_test.go` | workflow matrix | 119 |
| `embed/controlplane/controlplane_integration_test.go` | operator | 249 |
| `embed/host_context_isolation_integration_test.go` | authority | 154 |
| `embed/host_transactions_integration_test.go` | parity-archive | 275 |
| `embed/invoice_client_integration_test.go` | workflow matrix | 119 |
| `embed/invoice_sweep_args_integration_test.go` | workflow matrix | 145 |
| `embed/main_integration_test.go` | shared helper | 19 |
| `embed/manifest_mode_integration_test.go` | parity-archive | 672 |
| `embed/metering_client_integration_test.go` | workflow matrix | 94 |
| `embed/provider_obligation_client_integration_test.go` | spend | 404 |
| `embed/recovery_client_integration_test.go` | workflow matrix | 121 |
| `embed/runtime_rls_posture_integration_test.go` | authority | 73 |
| `embed/spend_delegations_integration_test.go` | spend | 95 |
| `examples/billingapp/billingapp_integration_test.go` | workflow matrix | 171 |
| `internal/cardguard/cardguard_test.go` | primitives | 241 |
| `internal/contractaudit/reachable_test.go` | primitives | 92 |
| `internal/contractaudit/snapshot_test.go` | primitives | 382 |
| `internal/contractaudit/workflows_test.go` | primitives | 190 |
| `internal/db/merchant_rls_integration_test.go` | authority | 364 |
| `internal/db/rls_integration_test.go` | shared helper | 28 |
| `internal/integrationharness/api_key_host_binding_test.go` | authority | 45 |
| `internal/integrationharness/browser_auth_test.go` | authority | 79 |
| `internal/integrationharness/browser_tier_cors_test.go` | authority | 82 |
| `internal/integrationharness/cross_merchant_isolation_test.go` | shared helper | 31 |
| `internal/integrationharness/cross_merchant_surface_isolation_test.go` | authority | 382 |
| `internal/integrationharness/customer_identity_test.go` | workflow matrix | 127 |
| `internal/integrationharness/delegated_admission_seam_integration_test.go` | workflow matrix | 221 |
| `internal/integrationharness/delegation_http_test.go` | workflow matrix | 244 |
| `internal/integrationharness/host_events_test.go` | operator | 184 |
| `internal/integrationharness/host_merchant_test.go` | shared helper | 66 |
| `internal/integrationharness/hosted_workflows_test.go` | workflow matrix | 352 |
| `internal/integrationharness/main_test.go` | shared helper | 13 |
| `internal/integrationharness/merchant_api_keys_http_test.go` | shared helper | 40 |
| `internal/integrationharness/merchant_archive_auth_test.go` | authority | 198 |
| `internal/integrationharness/merchant_catalog_http_test.go` | shared helper | 59 |
| `internal/integrationharness/merchant_payment_provider_archive_http_test.go` | operator | 387 |
| `internal/integrationharness/money_workflow_matrix_test.go` | workflow matrix | 307 |
| `internal/integrationharness/nmi_tier_change_matrix_test.go` | workflow matrix | 142 |
| `internal/integrationharness/standalone_no_default_merchant_test.go` | shared helper | 31 |
| `internal/integrationharness/stripe_tier_change_matrix_test.go` | workflow matrix | 115 |
| `internal/integrations/solana/partial_sign_test.go` | primitives | 88 |
| `internal/integrations/solana/transfer_verify_test.go` | primitives | 89 |
| `internal/intents/integration_test.go` | shared helper | 33 |
| `internal/intents/main_integration_test.go` | shared helper | 14 |
| `internal/intents/manual_rebill_finalize_integration_test.go` | existing rebill invariants; #990 gap remains | 124 |
| `internal/intents/manual_rebill_integration_test.go` | shared helper | 194 |
| `internal/intents/rail_clients_fake_test.go` | shared helper | 26 |
| `internal/intents/refund_cancellation_integration_test.go` | commerce | 87 |
| `internal/intents/refund_integration_test.go` | commerce | 532 |
| `internal/intents/refund_test.go` | shared helper | 27 |
| `internal/intents/stored_credential_rebill_integration_test.go` | shared helper | 41 |
| `internal/merchantarchive/adversarial_integration_test.go` | parity-archive | 316 |
| `internal/merchantarchive/archive_integration_test.go` | parity-archive | 222 |
| `internal/merchantarchive/purchase_workflow_integration_test.go` | parity-archive | 344 |
| `internal/modules/checkout/custodian_sale_integration_test.go` | commerce | 526 |
| `internal/modules/checkout/main_test.go` | shared helper | 24 |
| `internal/modules/checkout/model_b_overflow_test.go` | primitives | 59 |
| `internal/modules/checkout/operator_resolution_integration_test.go` | shared helper | 22 |
| `internal/modules/checkout/purchase_service_integration_test.go` | shared helper | 78 |
| `internal/modules/checkout/rail_target_resolution_test.go` | shared helper | 54 |
| `internal/modules/checkout/stripe_tier_change_integration_test.go` | uncertainty | 1010 |
| `internal/modules/checkout/tier_change_crossrail_integration_test.go` | uncertainty | 116 |
| `internal/modules/checkout/upgrade_adopt_integration_test.go` | shared helper | 297 |
| `internal/modules/checkout/upgrade_receipts_integration_test.go` | uncertainty | 535 |
| `internal/modules/checkout/upgrade_request_cancellation_integration_test.go` | uncertainty | 123 |
| `internal/modules/metrics/balance_overflow_integration_test.go` | primitives | 54 |
| `internal/modules/metrics/main_test.go` | shared helper | 11 |
| `internal/modules/metrics/metrics_integration_test.go` | shared helper | 26 |
| `internal/modules/money/arrears_cas_integration_test.go` | spend | 138 |
| `internal/modules/money/capture_overadmit_integration_test.go` | spend | 127 |
| `internal/modules/money/invoice_collection_integration_test.go` | shared helper | 96 |
| `internal/modules/money/invoice_collection_nmi_receipt_integration_test.go` | uncertainty | 571 |
| `internal/modules/money/invoice_collection_stripe_integration_test.go` | uncertainty | 638 |
| `internal/modules/money/ledger/ledger_integration_test.go` | shared helper | 109 |
| `internal/modules/money/ledger/replay_integration_test.go` | spend | 212 |
| `internal/modules/money/live_rail_invoice_integration_test.go` | shared helper | 59 |
| `internal/modules/money/main_test.go` | shared helper | 28 |
| `internal/modules/money/merchant_store_arming_integration_test.go` | shared helper | 70 |
| `internal/modules/money/merchant_store_collection_integration_test.go` | shared helper | 43 |
| `internal/modules/money/metered_watermark_integration_test.go` | spend | 171 |
| `internal/modules/money/money_in_integration_test.go` | shared helper | 309 |
| `internal/modules/money/nmi_collection_sandbox_integration_test.go` | manual provider qualification | 169 |
| `internal/modules/solana/recurring/atomic_subscribe_test.go` | primitives | 341 |
| `internal/modules/solana/recurring/plan_service_ata_test.go` | shared helper | 19 |
| `internal/modules/solana/recurring/prepare_subscribe_reference_test.go` | shared helper | 25 |
| `internal/modules/solana/recurring/prepare_tier_change_test.go` | shared helper | 35 |
| `internal/modules/solana/recurring/wiring_integration_test.go` | shared helper | 23 |
| `internal/reconcile/account_updater_integration_test.go` | operator | 174 |
| `internal/reconcile/engine_test.go` | shared helper | 36 |
| `internal/reconcile/integration_test.go` | shared helper | 128 |
| `internal/reconcile/maintenance_runs_integration_test.go` | operator | 90 |
| `internal/reconcile/prune_soft_delete_integration_test.go` | shared helper | 102 |
| `internal/reconcile/psp_binding_testhelper_integration_test.go` | shared helper | 41 |
| `internal/river/main_test.go` | shared helper | 15 |
| `internal/river/worker_state_integration_test.go` | operator | 381 |
| `internal/river/worker_sweep_correctness_integration_test.go` | operator | 211 |
| `internal/service/admission_client_workflow_integration_test.go` | workflow matrix | 177 |
| `internal/service/main_test.go` | shared helper | 14 |
| `internal/shared/moneyutil/conversion_bounds_test.go` | primitives | 58 |
| `internal/shared/moneyutil/currency_test.go` | primitives | 198 |
| `internal/shared/moneyutil/moneyutil_test.go` | primitives | 51 |
| `pkg/pricing/chargemodel_test.go` | primitives | 125 |
| `wire_contract_test.go` | primitives | 146 |

Initial planned Go test source: **17,640 lines in 107 files**. Whole-file removals: **847**. Final measured totals and test receipts will replace this preliminary count before readiness.
