package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/providerposture"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/retry"
	"github.com/open-rails/openrails/internal/writeposture"
)

// StartProviderPosture verifies, in the background, every PSP credential set
// of the merchants this runtime loads at startup (its configured merchant plus
// the declared ones), then re-verifies unknown verdicts with capped
// full-jitter backoff until each is known. Construction never waits on a
// provider. A PSP not proven to match the posture stays disarmed (its
// mutations are refused) while reads and other PSPs keep working; Ready
// reports it as degraded. Credentials loaded later are verified when written
// or on their first mutation.
//
// Under live posture every NMI account must prove it is not in test mode
// (SEC-33): an account left in test mode approves without moving money.
func (r *Runtime) StartProviderPosture(declared ...billing.MerchantID) {
	if r == nil || r.Config == nil || config.IsProviderReadOnly(r.Config) || r.DB == nil || r.Merchants == nil {
		if r != nil {
			r.posturePending.Store(0)
		}
		return
	}
	r.posturePending.Store(-1)
	r.Go("provider posture", func(ctx context.Context) {
		for attempt := 0; ; attempt++ {
			pending := r.verifyProviderPosture(ctx, declared)
			r.posturePending.Store(int64(pending))
			if pending == 0 || !retry.Sleep(ctx, retry.Backoff(attempt, time.Second, retry.Max)) {
				return
			}
		}
	})
}

// verifyProviderPosture verifies every loaded PSP whose verdict is not yet
// known and returns how many remain unknown.
func (r *Runtime) verifyProviderPosture(ctx context.Context, declared []billing.MerchantID) int {
	live := !config.IsTestMode(r.Config)
	environment := config.ExpectedProviderEnvironment(!live)
	registry := providerposture.Process()
	pending := 0
	seen := map[billing.MerchantID]bool{}
	for _, mid := range append([]billing.MerchantID{r.ConfiguredMerchant()}, declared...) {
		if mid.IsZero() || seen[mid] {
			continue
		}
		seen[mid] = true
		var ids []uuid.UUID
		if err := r.DB.RunInMerchantScope(ctx, mid, "provider posture", func(mctx context.Context) error {
			rows, err := r.DB.Gen(mctx).ListPSPsForMerchant(mctx, mid.UUID())
			if err != nil {
				return err
			}
			for _, row := range rows {
				if !row.Archived && row.Environment == environment && (!live || row.Rail == string(models.RailNMI)) {
					ids = append(ids, row.ID)
				}
			}
			return nil
		}); err != nil {
			if ctx.Err() == nil {
				log.WithContext(ctx).WithError(err).WithField("merchant_id", mid.String()).Error("provider posture: cannot read merchant PSPs")
			}
			pending++
			continue
		}
		// One unanswering provider must not hold the others' verdicts.
		var wg sync.WaitGroup
		var unknown atomic.Int64
		for _, id := range ids {
			if status, ok := r.providerPosture.PSPStatus(registry, id); ok && status.Verdict != providerposture.Unknown {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				vctx, cancel := context.WithTimeout(ctx, postureCheckTimeout)
				defer cancel()
				if !r.verifyPSPPosture(merchant.WithID(vctx, mid), mid, id) {
					unknown.Add(1)
				}
			}()
		}
		wg.Wait()
		pending += int(unknown.Load())
	}
	return pending
}

const postureCheckTimeout = 30 * time.Second

// verifyPSPPosture reports whether the PSP's verdict is now known.
func (r *Runtime) verifyPSPPosture(ctx context.Context, mid billing.MerchantID, pspID uuid.UUID) bool {
	svc := r.Merchants
	scope, ok, err := svc.PSPScopeByID(ctx, mid, pspID)
	fields := log.Fields{"merchant_id": mid.String(), "psp_id": pspID.String()}
	if err != nil || !ok {
		log.WithContext(ctx).WithError(err).WithFields(fields).Error("provider posture: PSP unavailable")
		return false
	}
	fields["rail"], fields["account_id"] = scope.Rail, scope.AccountID
	var status providerposture.Status
	var check providerposture.Check
	switch scope.Rail {
	case string(models.RailNMI):
		armer := &railresolve.NMIArmer{Config: r.Config, DB: r.DB, MerchantsFn: func() *merchants.Service { return svc }, Factory: r.NMIClients}
		client, err := armer.NMIClient(ctx, mid, scope)
		if err != nil {
			log.WithContext(ctx).WithError(err).WithFields(fields).Error("provider posture: NMI credentials unavailable; PSP disarmed")
			return !errors.Is(err, merchants.ErrSecretBackendUnavailable)
		}
		if client.LoopbackFixture && client.TestMode {
			return true
		}
		if r.NMIPostureV5BaseURL != "" {
			client.V5BaseURL = r.NMIPostureV5BaseURL
		}
		status, check = client.VerifyPosture(ctx), client.PostureCheck()
	case string(models.RailStripe):
		secret, err := pspSecret(ctx, svc, mid, scope, "secret_key")
		if err != nil {
			log.WithContext(ctx).WithError(err).WithFields(fields).Error("provider posture: Stripe credentials unavailable; PSP disarmed")
			return !errors.Is(err, merchants.ErrSecretBackendUnavailable)
		}
		status, check = r.StripeClients.VerifyPosture(ctx, secret, scope.AccountID), r.StripeClients.PostureCheckFor(secret, scope.AccountID)
	case string(models.RailSolana):
		if r.SolanaRPCResolver == nil || r.SolanaRPCResolver.Endpoint != "" {
			return true
		}
		client, err := r.SolanaRPCResolver.Resolve(ctx, mid)
		if err != nil || client == nil {
			log.WithContext(ctx).WithError(err).WithFields(fields).Error("provider posture: Solana RPC unavailable; PSP disarmed")
			return false
		}
		status, check = client.VerifyPosture(ctx), client.CheckPosture
	case string(models.RailCCBill):
		log.WithContext(ctx).WithFields(fields).Warn("provider posture: CCBill has no authoritative test-mode signal; its mutations are refused under sandbox posture")
		return true
	default:
		return true
	}
	r.providerPosture.AddPSP(pspID, status.Key, check)
	switch {
	case status.Armed():
		log.WithContext(ctx).WithFields(fields).Info("provider posture: credentials verified; PSP armed")
	case status.Verdict == providerposture.Unknown:
		log.WithContext(ctx).WithError(status.Error()).WithFields(fields).Warn("provider posture: provider did not answer; PSP disarmed until it does, retrying in the background")
	default:
		log.WithContext(ctx).WithError(status.Error()).WithFields(fields).Error("provider posture: credentials not verified; PSP disarmed")
	}
	return status.Verdict != providerposture.Unknown
}

// PSPPostureDisarmed reports whether pspID failed its posture verification.
func (r *Runtime) PSPPostureDisarmed(pspID uuid.UUID) bool {
	return r != nil && r.providerPosture.PSPDisarmed(providerposture.Process(), pspID)
}

// postureState summarizes the cached posture of the loaded PSPs without
// contacting any provider: nil when every one is verified and armed.
func (r *Runtime) postureState() error {
	pending := r.posturePending.Load()
	unarmed := r.providerPosture.Unarmed(providerposture.Process())
	if pending == 0 && len(unarmed) == 0 {
		return nil
	}
	reasons := make([]string, 0, len(unarmed)+1)
	switch {
	case pending < 0:
		reasons = append(reasons, "first verification pending")
	case pending > 0:
		reasons = append(reasons, fmt.Sprintf("%d awaiting a provider answer", pending))
	}
	for _, s := range unarmed {
		reasons = append(reasons, s.Error().Error())
	}
	return fmt.Errorf("PSPs not armed: %s", strings.Join(reasons, "; "))
}

func pspSecret(ctx context.Context, svc *merchants.Service, mid billing.MerchantID, scope merchants.PSPScope, key string) (string, error) {
	ref, err := scope.SecretRef(key)
	if err != nil {
		return "", err
	}
	sec, err := merchants.ReadSecretRef(ctx, svc.Secrets(), mid, ref)
	if err != nil {
		return "", err
	}
	if value := strings.TrimSpace(sec.Value); value != "" {
		return value, nil
	}
	return "", merchants.ErrSecretNotFound
}

// CheckBookIdentity says loudly when this database is a copy of the billing
// book: every merchant's provider writes stay readonly until it is armed.
func (r *Runtime) CheckBookIdentity(ctx context.Context) {
	if r == nil || r.DB == nil {
		return
	}
	armed, err := writeposture.BookArmed(ctx, r.DB.GenDirectory())
	switch {
	case err != nil:
		log.WithContext(ctx).WithError(err).Error("billing book identity unreadable: provider writes stay readonly")
	case !armed:
		log.WithContext(ctx).Error("this database is a copy of the billing book (another cluster, database or schema than the one it was armed in): " +
			"every provider write stays readonly. Once every other copy is stopped, run `openrails book arm --by NAME`")
	}
}

// StartCredentialFingerprints records, in the background, the credential
// fingerprint of each loaded merchant's PSPs that have none yet, so a gateway
// account declared twice before fingerprints existed is caught too.
func (r *Runtime) StartCredentialFingerprints(declared ...billing.MerchantID) {
	if r == nil || r.Merchants == nil {
		return
	}
	svc := r.Merchants
	r.Go("credential fingerprints", func(ctx context.Context) {
		seen := map[billing.MerchantID]bool{}
		for _, mid := range append([]billing.MerchantID{r.ConfiguredMerchant()}, declared...) {
			if mid.IsZero() || seen[mid] {
				continue
			}
			seen[mid] = true
			if err := svc.FingerprintPSPs(merchant.WithID(ctx, mid), mid); err != nil && ctx.Err() == nil {
				log.WithContext(ctx).WithError(err).WithField("merchant_id", mid.String()).Error("credential fingerprints: cannot record; duplicate gateway accounts are checked at the next publication")
			}
		}
	})
}
