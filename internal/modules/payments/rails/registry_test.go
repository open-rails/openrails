package rails

import (
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
)

// TestRegistryCompleteness: every enum rail has a descriptor and every
// descriptor's required fields are filled. The unkeyed literals already force
// every FIELD at compile time; this forces every RAIL.
func TestRegistryCompleteness(t *testing.T) {
	enum := []models.Rail{models.RailNMI, models.RailCCBill, models.RailSolana, models.RailStripe, models.RailPayPal}
	if len(descriptors) != len(enum) {
		t.Fatalf("registry has %d descriptors, enum has %d rails", len(descriptors), len(enum))
	}
	for _, rail := range enum {
		d, ok := Lookup(rail)
		if !ok {
			t.Fatalf("rail %q has no descriptor", rail)
		}
		if d.Rail != rail {
			t.Errorf("descriptor for %q carries rail %q", rail, d.Rail)
		}
		if strings.TrimSpace(d.DisplayName) == "" {
			t.Errorf("rail %q: DisplayName is empty", rail)
		}
		if d.AutoBilled == nil {
			t.Errorf("rail %q: AutoBilled func is nil", rail)
		}
		if d.CancelMode == nil {
			t.Errorf("rail %q: CancelMode func is nil", rail)
		}
		if d.CancelPortalURL != "" && d.CancelMode(&models.Subscription{Rail: rail}, time.Now()) != CancelModeExternalPortal {
			t.Errorf("rail %q: CancelPortalURL set but cancel mode is not external_portal", rail)
		}
		for _, k := range d.CredentialKeys {
			if strings.TrimSpace(k.Name) == "" {
				t.Errorf("rail %q: empty credential key name", rail)
			}
			if k.Name != strings.ToLower(k.Name) {
				t.Errorf("rail %q: credential key %q not lower-case", rail, k.Name)
			}
		}
	}
}

// TestRegistryPinnedFacts pins the per-rail facts the old switches encoded, so
// a descriptor edit that flips behavior fails here first.
func TestRegistryPinnedFacts(t *testing.T) {
	cases := []struct {
		rail                 models.Rail
		remoteCustomer       bool
		chargeSaved          bool
		openRailsDunning     bool
		renewalGrace         bool
		railMerchantAccounts bool
		merchantKeyCount     int
		autoBilledNilPM      bool
		autoBilledWithMethod bool
		remoteDeleteTerminal bool
		activeCancelMode     CancelMode
	}{
		{models.RailNMI, false, true, true, true, true, 2, true, false, true, CancelModeDestructive}, // remoteCustomer=false per #682
		{models.RailCCBill, false, false, false, false, true, 3, true, true, false, CancelModeExternalPortal},
		{models.RailStripe, true, true, false, true, true, 3, false, false, false, CancelModeReversible},
		{models.RailSolana, false, false, true, false, true, 0, false, false, false, CancelModeDestructive},
		{models.RailPayPal, false, false, false, false, false, 0, false, false, false, CancelModeDestructive},
	}
	// #682: the rebill-driver mode is EXPLICIT now — a method ref alone no longer
	// flips NMI to our-rebill; RebillDriver does.
	withMethod := &models.PaymentMethod{RailMethodRef: "vault-123", RebillDriver: models.RebillDriverOpenRails}
	for _, c := range cases {
		d, ok := Lookup(c.rail)
		if !ok {
			t.Fatalf("no descriptor for %q", c.rail)
		}
		if d.HasRemoteCustomer != c.remoteCustomer {
			t.Errorf("%s: HasRemoteCustomer = %v", c.rail, d.HasRemoteCustomer)
		}
		if d.SupportsChargeSavedMethod != c.chargeSaved {
			t.Errorf("%s: SupportsChargeSavedMethod = %v", c.rail, d.SupportsChargeSavedMethod)
		}
		if d.OpenRailsDrivenDunning != c.openRailsDunning {
			t.Errorf("%s: OpenRailsDrivenDunning = %v", c.rail, d.OpenRailsDrivenDunning)
		}
		if d.RenewalGraceEligible != c.renewalGrace {
			t.Errorf("%s: RenewalGraceEligible = %v", c.rail, d.RenewalGraceEligible)
		}
		if d.HasRailMerchantAccounts != c.railMerchantAccounts {
			t.Errorf("%s: HasRailMerchantAccounts = %v", c.rail, d.HasRailMerchantAccounts)
		}
		if got := len(MerchantCredentialKeyNames(c.rail)); got != c.merchantKeyCount {
			t.Errorf("%s: %d merchant-writable keys, want %d", c.rail, got, c.merchantKeyCount)
		}
		if got := AutoBilled(c.rail, nil); got != c.autoBilledNilPM {
			t.Errorf("%s: AutoBilled(nil) = %v", c.rail, got)
		}
		if got := AutoBilled(c.rail, withMethod); got != c.autoBilledWithMethod {
			t.Errorf("%s: AutoBilled(withMethod) = %v", c.rail, got)
		}
		if d.RemoteDeleteOnTerminalCancel != c.remoteDeleteTerminal {
			t.Errorf("%s: RemoteDeleteOnTerminalCancel = %v", c.rail, d.RemoteDeleteOnTerminalCancel)
		}
		if got := CancelModeFor(&models.Subscription{Rail: c.rail, Status: models.StatusActive}, time.Now()); got != c.activeCancelMode {
			t.Errorf("%s: CancelModeFor(active) = %v, want %v", c.rail, got, c.activeCancelMode)
		}
	}

	// CCBill is the only external-portal rail and carries the portal URL.
	if got := CancelPortalURL(models.RailCCBill); got != "https://support.ccbill.com" {
		t.Errorf("ccbill CancelPortalURL = %q", got)
	}

	// NMI's cancel mode is state-conditional (issue 216): reversible ONLY while
	// cancelled + deferred delete pending + paid period in the future.
	now := time.Now()
	future := now.Add(72 * time.Hour)
	past := now.Add(-time.Hour)
	pending := &models.Subscription{Rail: models.RailNMI, Status: models.StatusCancelled, DeletionScheduledAt: &future, CurrentPeriodEndsAt: &future}
	if got := CancelModeFor(pending, now); got != CancelModeReversible {
		t.Errorf("nmi delete-pending cancel mode = %v, want reversible", got)
	}
	executed := &models.Subscription{Rail: models.RailNMI, Status: models.StatusCancelled, CurrentPeriodEndsAt: &future} // marker cleared by the finalizer
	if got := CancelModeFor(executed, now); got != CancelModeDestructive {
		t.Errorf("nmi delete-executed cancel mode = %v, want destructive", got)
	}
	lapsed := &models.Subscription{Rail: models.RailNMI, Status: models.StatusCancelled, DeletionScheduledAt: &future, CurrentPeriodEndsAt: &past}
	if got := CancelModeFor(lapsed, now); got != CancelModeDestructive {
		t.Errorf("nmi lapsed-period cancel mode = %v, want destructive", got)
	}

	// Solana's signer is operator-only: present in the full set, absent from
	// the merchant view (#669 note D).
	if k, ok := CredentialKeyFor(models.RailSolana, "private_key"); !ok || k.MerchantWritable {
		t.Errorf("solana private_key: ok=%v merchantWritable=%v, want operator-only", ok, k.MerchantWritable)
	}
}

// TestLookupNormalizes pins the normalization contract and unknown-rail defaults.
func TestLookupNormalizes(t *testing.T) {
	if _, ok := Lookup(" STRIPE "); !ok {
		t.Error("Lookup must normalize case/whitespace")
	}
	// #630: mobius is a provider-account name on rail nmi, not a rail.
	if _, ok := Lookup("mobius"); ok {
		t.Error("mobius must not resolve to a descriptor")
	}
	if HasRemoteCustomer("bogus") || SupportsRailMerchantAccounts("bogus") || AutoBilled("bogus", nil) || RenewalGraceEligible("bogus") || RemoteDeleteOnTerminalCancel("bogus") {
		t.Error("unknown rails must default to false")
	}
	if got := CancelModeFor(&models.Subscription{Rail: "bogus"}, time.Now()); got != CancelModeDestructive {
		t.Errorf("unknown-rail cancel mode = %v, want destructive (safe default)", got)
	}
	if CancelModeFor(nil, time.Now()) != CancelModeDestructive {
		t.Error("nil-subscription cancel mode must be destructive")
	}
	if CancelPortalURL("bogus") != "" {
		t.Error("unknown-rail portal URL must be empty")
	}
	if got := DisplayName("bogus"); got != "BOGUS" {
		t.Errorf("DisplayName fallback = %q, want BOGUS", got)
	}
	if got := DisplayName(""); got != "" {
		t.Errorf("DisplayName(\"\") = %q, want empty", got)
	}
	if got := DisplayName(models.RailStripe); got != "Stripe" {
		t.Errorf("DisplayName(stripe) = %q", got)
	}
}
