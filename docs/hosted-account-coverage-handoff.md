# Hosted account coverage handoff

Status: draft integration. The removal source is frozen at `f535f36c`. This branch
moves its two narrow server checks into the existing critical harness and removes
only obsolete core product tests. Full combined qualification and the destination
browser/lifecycle receipts are still required before merge.

## Existing obligations and their destination

| Retired core case | SaaS replacement and evidence | Remaining boundary |
|---|---|---|
| OR-J02-S01 both identities / one-use proof / native-account distinction | Controlled linking `6da9c749`: TestControlledCustomerLinkRequiresBothProofsAndConsumesChallenge and TestControlledCustomerLinkProtectsRealNativeAccountsAndTombstones, selected real-service race receipts | Combined current core/SaaS HTTP activation |
| OR-J02-S02 challenge binding, expiry, conflicts, rollback | Controlled linking `6da9c749`: TestControlledCustomerLinkBindsBothSidesAndRollsBackTogether, exact selected race receipt | Combined current cohort |
| OR-J02-S03 only owned mapped customer authority | Account routes `65d7090`: TestAccountHTTPRoutesUseOnlyOwnedActiveProof, real HTTP/TS race in 15.172s; generic J01-S09 retained in core | Final production routing and portal browser |
| OR-J02-S04 recent sign-in before unlink | SaaS lifecycle `e1c7f00`: TestAccountLifecycleUnlinkRequiresOwnedRecentProofAndNoRebill (two-guard race run in 25.286s) | Final frozen receipt and actual callback/HTTP composition pending |
| OR-J02-S05 in-use billing guard and history preservation | SaaS lifecycle `e1c7f00`: TestAccountLifecycleOwnedUnlinkRetainsBillingAndOneFact and live-rebill guard (two-guard race run in 25.286s) | Final frozen receipt and callback/reader composition pending |
| OR-UI-06 actual consented browser linking | SaaS portal/browser owner | Pending: Go service checks are not browser evidence |
| Network-link calls inside OR-UI-03 | Covered by SaaS linking/HTTP/browser replacement | Remaining checkout-error assertions stay in core UI03 |

Original core IDs are retired with these mappings, not reused for new meanings.
`critical/coverage.json` contains OR-J IDs only; UI06 is one case inside the
retained OR-J04-S01 browser journey, not an extra ID in that Go manifest.

## New core obligations

- OR-J01-S16: optional host policy refuses ordinary customer admission before
  allocation, including existing rows; staff, mapped and pay-URL policy boundaries
  remain distinct. Explicit host OAuth scope works and cannot broaden canonical
  self scope into administration. Historical raw network facts remain readable.
- OR-J31-S05: first application of the removal migration refuses populated or
  ambiguous ownership, fails immediately on busy sources, preserves a transferred
  opaque cursor, and accepts fresh/repeated correctly recorded state.

Migration checks live in the root critical package. Real server checks use the
existing standalone fixture/commands. No parallel CI job or duplicate suite is
added. The provisional Go count is computed from exact ID sets:155 minus the five
J02 IDs plus the two new IDs =152. Browser selection drops UI06 only; all ordinary
UI03 checkout errors, J04 transport retry/cancellation, and money outcomes remain.
J01-S09 and J07-S05 retain their original generic authority/billing obligations
with absence assertions instead of creating or renaming removed core storage.

## Current harness qualification

The root critical package and shared standalone fixture compile after retiring
only the product APIs. Four selected Go scenarios / five required paths pass
in 13.736s (ordinary execution): new admission/migration obligations plus both
J01-S09 mapping profiles and J07-S05 billing reads. The strict executable ledger
accepts every selected path exactly once. The unchanged production SDK artifact
was copied from the qualified `f535` source; the changed browser harness is rebuilt
and typechecks. The retained SDK browser subset also passes in 17.908s across J04-S01 and
J04-S03 (five real Chromium cases, zero method-ledger errors); final destination
receipts are still pending. No full-suite or race claim is made for this local run.

## Current release baseline

The extraction now includes OpenRails v0.267.0, including notification issue
ordering (migration 0115) and recurring service-credit billing. Migration 0116
keeps its original number and no published migration file changed. The current
roster is 153 logical scenarios and 189 required paths (156 current core scenarios,
minus five moved product cases, plus two extraction cases).

OR-J31-S05 also exercises public `openrails.Migrate`: populated reader state
refuses without advancing its migration ledger, explicit offline schema transfer
preserves the cursor, and repeated public migration then succeeds. Destination
SaaS qualification and restored-backup rehearsal still gate merge/release.
