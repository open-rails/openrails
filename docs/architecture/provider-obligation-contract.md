# Provider obligations and host transactions

The ordinary OpenRails Client opens, reads, and releases durable provider-operation authorizations, and records or reads provider billing qualification. These commands use the same request and result types in embedded, sidecar, and standalone deployments. OpenRails owns each command's commit.

An embedding host that must commit its own provider obligation or absence fact with a monetary transition uses the separate typed host transaction extension. It accepts the host's `pgx.Tx`, binds the configured merchant, and neither commits nor rolls back. Open, read, release, and provider observation/qualification must operate on that transaction. Remote Client calls cannot participate in an unrelated host database transaction, and an outbox is not a substitute for the accepted atomic obligation contract.

Authorization binds immutable operation identity, payer, claim reference, exact body bytes and digest, and authorized USD micros. Provider observations contain immutable raw responses, normalized provider records, and lifecycle evidence. OpenRails alone applies qualification, rates provider cost, and posts final settlement. There is no public caller-rated settlement command or public pass-through ledger primitive. Ambiguous provider creation leaves authorization open; proven non-creation can release it only before billing evidence exists.

The accepted Tensorhub th-005 contract requires the provider obligation and authorization to commit together, as well as provider non-creation and release. This OpenRails extension supplies that boundary; it does not arm or implement Tensorhub's customer billing product.

The proposed proto-022 growing authorization and proto-023 balance repayment policies remain deferred. Potential additive schema work includes ordinal authorization extension receipts, an authorized total distinct from the immutable opening amount, a repayment transfer type, and invoice-item allocation/netting state. None is required for the fixed opening authorization and one final evidence-qualified settlement contract, and none is introduced here without an owner ruling.
