// Package identity provides the explicit, mutually-distinct identity types that
// OpenRails billing uses so that admin authority, the payer account, and the
// invoking principal can never be confused.
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
//  2. TenantSubjectID. The AuthKit org/personal-org whose credit balance is charged,
//     against which invoices and credit reservations are recorded. Every account
//     is org-backed: an individual user's balance is paid by that user's personal
//     AuthKit org, and team orgs use the same payer primitive. Bills and
//     balances belong to TenantSubjectIDs. The payer org id is supplied by the caller
//     at write time — it is the real AuthKit org id, never a synthesized stand-in.
//
//  3. InvokerID. The user / OpenRails-issued OAT / delegated platform user /
//     system principal that invoked a billable operation for a payer org.
//     Invokers cause usage; they are NOT the financial account owner. Invoker
//     identity is attribution + budget scoping, not ownership.
//
// Mnemonic: the OperatorOrg administers; the TenantSubjectID pays; the InvokerID
// spends. Bills and balances belong to orgs; users and credentials cause usage
// inside orgs.
package identity

import (
	"strings"

	"github.com/google/uuid"
)

// TenantSubjectID is the AuthKit org/personal-org that owns a credit balance and is
// billed. It is a distinct type from any invoker or operator identity precisely
// so the compiler rejects passing the wrong one. The value is the real AuthKit org id,
// supplied by the caller at write time — there is no synthesized placeholder.
type TenantSubjectID uuid.UUID

// InvokerID is the user / OAT / delegated principal that invoked usage.
// It is attribution + budgeting, never ownership. Stored as the existing free
// -form user_id text (may be a uuid string, a username, or a system actor key),
// so it is a string-backed type rather than uuid-backed.
type InvokerID string

// String returns the canonical string form of the payer org id.
func (id TenantSubjectID) String() string { return uuid.UUID(id).String() }

// UUID returns the underlying uuid.UUID for use in queries.
func (id TenantSubjectID) UUID() uuid.UUID { return uuid.UUID(id) }

// IsZero reports whether the payer org id is unset.
func (id TenantSubjectID) IsZero() bool { return uuid.UUID(id) == uuid.Nil }

// String returns the invoker id as a plain string.
func (a InvokerID) String() string { return string(a) }

// IsZero reports whether the invoker id is empty.
func (a InvokerID) IsZero() bool { return strings.TrimSpace(string(a)) == "" }

// TenantSubjectIDFromString parses s as the canonical payer AuthKit org id. It is the
// caller-side resolution used by self-hosted / single-tenant callers whose payer
// org id is the user's own personal-org / account UUID: the caller passes the
// authenticated subject and gets back the TenantSubjectID. An empty or non-UUID input
// yields the zero TenantSubjectID (which callers must treat as "no payer resolved").
func TenantSubjectIDFromString(s string) TenantSubjectID {
	s = strings.TrimSpace(s)
	if s == "" {
		return TenantSubjectID(uuid.Nil)
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return TenantSubjectID(uuid.Nil)
	}
	return TenantSubjectID(id)
}
