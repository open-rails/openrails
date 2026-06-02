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
//     user's balance is owned by THAT user's personal AuthKit org, and team orgs
//     use the same owner primitive. Bills and balances belong to OwnerOrgIDs.
//     The owner org id is supplied by the caller at write time — it is the real
//     owner AuthKit org id, never a synthesized stand-in.
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

// OwnerOrgID is the org that OWNS a credit balance / is billed (#221). It is a
// distinct type from any actor or operator identity precisely so the compiler
// rejects passing the wrong one. The value is the real owner AuthKit org id,
// supplied by the caller at write time — there is no synthesized placeholder.
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

// OwnerOrgIDFromString parses s as the canonical owner AuthKit org id. It is the
// caller-side resolution used by self-hosted / single-tenant callers whose owner
// org id is the user's own personal-org / account UUID: the caller passes the
// authenticated subject and gets back the OwnerOrgID. An empty or non-UUID input
// yields the zero OwnerOrgID (which callers must treat as "no owner resolved").
func OwnerOrgIDFromString(s string) OwnerOrgID {
	s = strings.TrimSpace(s)
	if s == "" {
		return OwnerOrgID(uuid.Nil)
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return OwnerOrgID(uuid.Nil)
	}
	return OwnerOrgID(id)
}
