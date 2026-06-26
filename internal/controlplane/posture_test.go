package controlplane

import (
	"testing"

	authcore "github.com/open-rails/authkit/embedded"
)

// TestPrivatePostureIsLockedInThisRepo guards the default "private by
// construction" invariant: standalone never passes WithHostedPosture.
func TestPrivatePostureIsLockedInThisRepo(t *testing.T) {
	cp := &ControlPlane{}
	if !cp.SelfHostedPosture() {
		t.Fatal("SelfHostedPosture() must be true in this repo: open/public registration belongs in openrails-saas, not here")
	}
	if got := cp.MountedRouteGroups(); len(got) != len(IntentionalRouteGroups) {
		t.Fatalf("MountedRouteGroups() len = %d, want %d", len(got), len(IntentionalRouteGroups))
	}

	if got := registrationMode(true); got != authcore.RegistrationModeClosed {
		t.Fatalf("registrationMode(true) = %q, want %q (no public self-registration in this repo)",
			got, authcore.RegistrationModeClosed)
	}
}

func TestHostedPostureIsCodeOnlyOptIn(t *testing.T) {
	opts := newOptions([]Option{WithHostedPosture()})
	if !opts.hosted {
		t.Fatal("WithHostedPosture did not set hosted posture")
	}

	cp := &ControlPlane{hosted: true}
	if cp.SelfHostedPosture() {
		t.Fatal("hosted posture should not report self-hosted")
	}
	if got := cp.MountedRouteGroups(); got != nil {
		t.Fatalf("MountedRouteGroups() = %#v, want nil for AuthKit DefaultAPI", got)
	}
	if got := registrationMode(false); got != authcore.RegistrationModeOpen {
		t.Fatalf("registrationMode(false) = %q, want %q", got, authcore.RegistrationModeOpen)
	}
}
