package controlplane

import (
	"testing"

	authcore "github.com/open-rails/authkit/core"
)

// TestPrivatePostureIsLockedInThisRepo guards the "private by construction"
// invariant documented on SelfHostedPosture and registrationMode: standalone
// OpenRails in THIS repo never exposes public/open self-registration of users or
// merchants, and never widens /auth/* to the AuthKit DefaultAPI surface
// (RouteRegister / RouteOrganizations).
//
// Public, hosted, self-serve registration is owned ENTIRELY by the separate,
// private openrails-saas repo, which overrides this posture. If this test fails,
// someone made the posture or registration mode configurable here — that change
// almost certainly belongs in openrails-saas, not in this repo.
func TestPrivatePostureIsLockedInThisRepo(t *testing.T) {
	// Posture is hardcoded true → RouteSpecs() never returns DefaultAPI(), so the
	// public-onboarding AuthKit groups are never mounted on /auth/* here.
	if !(&ControlPlane{}).SelfHostedPosture() {
		t.Fatal("SelfHostedPosture() must be true in this repo: open/public registration belongs in openrails-saas, not here")
	}

	// Both registration call sites in New() pass locked=true → admin-bootstrap-only
	// (no public user self-registration, no public merchant/org onboarding).
	if got := registrationMode(true); got != authcore.RegistrationModeAdminBootstrapOnly {
		t.Fatalf("registrationMode(true) = %q, want %q (no public self-registration in this repo)",
			got, authcore.RegistrationModeAdminBootstrapOnly)
	}
}
