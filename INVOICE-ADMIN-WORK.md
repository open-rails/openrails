# OpenRails #802 invoice administration

Owner: Codex authkit_enrollment_fix agent
Issue: /home/fidika/cozy/.worktrees/tracker/audit-remediation-status/openrails/802.md
Branch: fix/invoice-admin
Base: ba3b1f9ad447b283e1743b6bf9f2c81783a4e24e (origin/master)
Worktree: /home/fidika/cozy/.worktrees/openrails/invoice-admin

Contract:
- Merchant list/detail/filter and paginated collection history over existing invoices.
- Read/update/collect invoice permissions; profile editing uses customer-settings permissions.
- Existing void, uncollectible, off-channel remittance, and durable keyed collection retry only. Unknown/in-flight outcomes cannot be blindly unparked or retried.
- Issuance stays with the existing billing-cycle engine; profile changes affect only future snapshots.
- Dedicated invoice pages/query/API modules. Invoice profile mounts on customer detail, coordinated with the credit-support owner.

No new invoice engine, tax calculator, PDF/CSV generator, or payment rail.
