// Package identity provides the explicit, mutually-distinct identity types that
// OpenRails billing uses so that admin authority, the payer/billing owner, and
// the usage actor can never be confused (issue #221).
//
// # THE THREE-WAY IDENTITY SPLIT
//
// OpenRails distinguishes three orthogonal identities. Conflating them is the
// single most dangerous class of bug in money-sensitive billing code, so each
// gets its OWN named type and they are intentionally NOT interchangeable:
//
//  1. OperatorOrg (admin authority — #224). The one OpenRails-owned AuthKit org
//     per deployment whose members may OPERATE the billing deployment: write the
//     catalog, issue refunds, grant entitlements, read metrics. It is named by
//     `auth.operator_org_slug`. It is NOT the owner of any customer's balance.
//     Admin authority is evaluated live against the operator org's effective
//     permissions (see internal/controlplane + internal/auth/policy), never from
//     a global `admin` role and never from stale JWT claims. This package does
//     not redefine the operator-org type — that authority lives in
//     internal/controlplane — it only documents where it sits in the split.
//
//  2. OwnerOrgID (the payer / billing owner — #221). The org that OWNS a credit
//     balance, is billed for usage, and against which invoices and credit
//     reservations are recorded. Every account is org-backed: an individual
//     user's balance is owned by THAT user's personal AuthKit org
//     (profiles.orgs.is_personal=true, owner_user_id=<user_id>), and team orgs
//     use the same owner primitive. Bills and balances belong to OwnerOrgIDs.
//
//  3. ActorUserID (who CAUSED the usage — #221). The user / OpenRails-issued
//     OAT / delegated platform user / system that caused usage inside an owner
//     org. Actors cause usage; they are NOT the financial account owner. Actor
//     identity is attribution + budgets, not ownership.
//
// Mnemonic: the OperatorOrg administers; the OwnerOrgID pays; the ActorUserID
// spends. Bills and balances belong to orgs; users and credentials cause usage
// inside orgs.
package identity

import (
	"strings"

	"github.com/google/uuid"
)

// personalOrgNamespace is the fixed UUID namespace used to DETERMINISTICALLY
// derive a stand-in personal-org id from an actor user id during the additive
// #221 rollout, BEFORE real AuthKit personal-org rows (profiles.orgs) are
// guaranteed to exist.
//
// DERIVATION RULE (must match the SQL backfill in migration 040 byte-for-byte):
//
//	PersonalOrgID(userID) = UUIDv5(personalOrgNamespace, "openrails:personal-org:" + userID)
//
// UUIDv5 (RFC 4122, SHA-1, name-based) is used because it is:
//   - deterministic: the same user id always maps to the same owner id, so the
//     migration backfill and live writes agree without coordination;
//   - collision-free across distinct user ids (SHA-1 name hashing);
//   - reproducible in pure SQL (pgcrypto digest) so the migration can backfill
//     existing rows without a round-trip to Go.
//
// This is a STAND-IN owner id, not the real AuthKit personal-org id (those are
// random uuidv7 minted by AuthKit's ensurePersonalOrgForUser and are not yet
// guaranteed to exist for legacy users). AuthKit-side provisioning must later
// reconcile each stand-in owner id to the user's real personal-org id; see the
// migration header and the #221 status notes for the coordination plan. Until
// then, balances are correctly org-scoped to a stable, per-user owner id.
var personalOrgNamespace = uuid.MustParse("6f1c9b3e-2a44-5d7c-8e10-9a2b3c4d5e6f")

// personalOrgPrefix namespaces the derivation input so the same user id can
// never collide with any other UUIDv5 derivation in the codebase.
const personalOrgPrefix = "openrails:personal-org:"

// OwnerOrgID is the org that OWNS a credit balance / is billed (#221). It is a
// distinct type from any actor or operator identity precisely so the compiler
// rejects passing the wrong one.
type OwnerOrgID uuid.UUID

// ActorUserID is the user / OAT / delegated principal that CAUSED usage (#221).
// It is attribution + budgeting, never ownership. Stored as the existing free
// -form user_id text (may be a uuid string, a username, or a system actor key),
// so it is a string-backed type rather than uuid-backed.
type ActorUserID string

// String returns the canonical string form of the owner org id.
func (id OwnerOrgID) String() string { return uuid.UUID(id).String() }

// UUID returns the underlying uuid.UUID for use in queries.
func (id OwnerOrgID) UUID() uuid.UUID { return uuid.UUID(id) }

// IsZero reports whether the owner id is unset.
func (id OwnerOrgID) IsZero() bool { return uuid.UUID(id) == uuid.Nil }

// String returns the actor user id as a plain string.
func (a ActorUserID) String() string { return string(a) }

// IsZero reports whether the actor id is empty.
func (a ActorUserID) IsZero() bool { return strings.TrimSpace(string(a)) == "" }

// PersonalOrgID deterministically derives the stand-in personal-org OwnerOrgID
// for an actor user id, per the DERIVATION RULE documented on
// personalOrgNamespace. It is the default billing owner for single-user callers
// that do not pass an explicit owner org, which is what keeps existing
// user-scoped callers working unchanged after #221.
//
// The input is the raw actor user id (trimmed). An empty input yields the zero
// OwnerOrgID, which callers must treat as "no owner resolved".
func PersonalOrgID(actorUserID string) OwnerOrgID {
	actorUserID = strings.TrimSpace(actorUserID)
	if actorUserID == "" {
		return OwnerOrgID(uuid.Nil)
	}
	return OwnerOrgID(uuid.NewSHA1(personalOrgNamespace, []byte(personalOrgPrefix+actorUserID)))
}

// PersonalOrgNamespace returns the fixed namespace UUID used by the derivation.
// Exposed so tests (and the SQL backfill cross-check) can assert byte-for-byte
// agreement between the Go and SQL derivations.
func PersonalOrgNamespace() uuid.UUID { return personalOrgNamespace }

// PersonalOrgPrefix returns the derivation input prefix, for the same
// cross-check reasons as PersonalOrgNamespace.
func PersonalOrgPrefix() string { return personalOrgPrefix }
