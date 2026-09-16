# Durable NMI upgrade recovery

An upgrade is one durable rail intent with immutable pricing, billing period, instrument and predecessor/successor identities. Each provider step records its submission boundary before calling NMI and retains a positive receipt or definitive refusal after the response. A restart resumes an unsent step; a possibly submitted step only reads provider evidence. Empty search results never permit resubmission.

The successor enrollment and proration charge have separate receipts. Once both are established, the predecessor cancellation, successor registration, payment record, access changes and deferred provider-deletion intent commit atomically. Local failure leaves the provider receipts intact so restart can repeat only the local transaction.

Provider search visibility and exact correlation remain adapter evidence; local loopback tests prove the application's no-resend and transaction behavior, not a live provider guarantee.
