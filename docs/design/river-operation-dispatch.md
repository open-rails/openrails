# Financial operation dispatch

Admission writes the accepted `rail_intents` row and inserts a typed River job in
one PostgreSQL transaction. The job carries merchant and operation IDs only.
`InsertTx` uses the caller's transaction, including nested savepoints; the runtime
supplies the host-composed client and its actual River schema. Missing composition
or a failed enqueue rolls admission back.

The request can attempt the operation immediately. The River job remains durable
while the ledger is pending, retryable, held, in flight, or unknown. Each dispatch
loads the ledger, waits until its recorded due time/live claim expiry, and applies
the existing authorization and submission fences. An abandoned in-flight claim
becomes unknown and gets provider readback before any permitted retry. No periodic
ledger scan is needed to wake ordinary accepted operations.

A terminal row completes the River job; the financial row and evidence remain.
If acknowledgement fails, another dispatch observes the terminal row and does
nothing. Nonterminal work snoozes to its next due time. A provider error never
becomes a financial decline just because a River retry failed. Recoverable
infrastructure failures and caught panics snooze without consuming River attempts.
River's native process-crash rescue limit is 32767 attempts (its maximum smallint);
exhaustion leaves a durable discarded River job for operator investigation/retry,
with the unresolved financial ledger intact. There is no new financial operation
or automatic blind resubmission at that limit.

Financial wakeups deliberately do not use River uniqueness. A running job may
already have observed a terminal row while an authorized generic deferred operation
is being revived, or a scheduled job may be waiting for an obsolete later time.
Suppressing the new wakeup can strand work. Duplicate wakeups are safe because the
ledger's claims, provider idempotency, and submission evidence remain authoritative.
`Store.WakeOperation` adds a prompt wakeup for an already accepted, scoped operation;
it changes neither authorization nor financial state.

Retry, read-only/kill-switch holds, and operator resolution retain the original
nonterminal job; it re-evaluates the committed state on its next wakeup. A new due
subscription period is still identified by a periodic domain worker, which admits
its operation and job atomically. Historical merchant archives currently reject
unresolved operations in preflight; restoring history does not activate background
financial mutations or silently create jobs for imported unresolved state.

Local verification covers transaction visibility and rollback with public/custom
River schemas and a one-connection admission pool, missing-producer rollback,
post-provider acceptance interruption followed by readback without another write,
duplicate delivery, generic revival racing prior job completion, and earlier
wakeups. Deterministic provider transports do not qualify live provider behavior.

The old `Runner.RunExecuteOnce`/`RunVerifyOnce` helpers remain solely to drive
existing regression fixtures during their conversion to individual operations;
there are no production callers or periodic registrations. Merchant fanout
methods and SQL readers are removed. Keeping the fixture helpers preserves
financial coverage rather than retaining an alternative runtime queue.
