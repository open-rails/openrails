# Hosted account coverage handoff

Status: draft integration. The removal source is frozen atf535f36c. This branch
moves its two narrow server checks into the existing critical harness and removes
only obsolete core product tests. Full combined qualification and the destination
browser/lifecycle receipts are still required before merge.

## Existing obligations and their destination

| Retired core case | SaaS replacement and evidence | Remaining boundary |
|---|---|---|
| OR-J02-S01 both identities / one-use proof / native-account distinction | Controlled linking6da9c749: TestControlledCustomerLinkRequiresBothProofsAndConsumesChallenge and TestControlledCustomerLinkProtectsRealNativeAccountsAndTombstones, selected real-service race receipts | Combined current core/SaaS HTTP activation |
| OR-J02-S02 challenge binding, expiry, conflicts, rollback | Controlled linking6da9c749: TestControlledCustomerLinkBindsBothSidesAndRollsBackTogether, exact selected race receipt | Combined current cohort |
| OR-J02-S03 only owned mapped customer authority | Account routes65d7090: TestAccountHTTPRoutesUseOnlyOwnedActiveProof, real HTTP/TS race15.172s; generic J01-S09 retained in core | Final production routing and portal browser |
| OR-J02-S04 recent sign-in before unlink | SaaS lifecycle worktree67-account-lifecycle, guard/unlink checks | Final frozen receipt and actual callback/HTTP composition pending |
| OR-J02-S05 in-use billing guard and history preservation | SaaS lifecycle worktree67-account-lifecycle, guard/unlink checks | Final frozen receipt and callback/reader composition pending |
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
