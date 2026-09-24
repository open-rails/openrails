package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/providerposture"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
)

// VerifyProviderPosture verifies, once, every sandbox PSP credential set of
// the merchants this runtime loads at startup (its configured merchant plus
// the declared ones). A PSP not proven to simulate money stays disarmed (its
// mutations are refused) and Ready reports it; the runtime still starts so
// reads and other PSPs keep working. Hosts that want a hard startup failure
// check Ready after construction. Credentials loaded later are verified when
// written or on their first mutation.
//
// Under live posture every NMI account must prove it is not in test mode
// (SEC-33): an account left in test mode approves without moving money.
func (r *Runtime) VerifyProviderPosture(ctx context.Context, declared ...merchant.ID) {
	if r == nil || r.Config == nil || r.Config.IsProviderReadOnly() || r.DB == nil || r.Merchants == nil {
		return
	}
	live := !r.Config.IsTestMode()
	environment := config.ExpectedProviderEnvironment(!live)
	seen := map[merchant.ID]bool{}
	for _, mid := range append([]merchant.ID{r.ConfiguredMerchant()}, declared...) {
		if mid.IsZero() || seen[mid] {
			continue
		}
		seen[mid] = true
		if err := r.DB.RunInMerchantScope(ctx, mid, "provider posture", func(mctx context.Context) error {
			rows, err := r.DB.Gen(mctx).ListPSPsForMerchant(mctx, gen.ListPSPsForMerchantParams{MerchantID: mid.UUID()})
			if err != nil {
				return err
			}
			for _, row := range rows {
				if !row.Archived && row.Environment == environment && (!live || row.Rail == string(models.RailNMI)) {
					r.verifyPSPPosture(mctx, mid, row.ID)
				}
			}
			return nil
		}); err != nil {
			log.WithContext(ctx).WithError(err).WithField("merchant_id", mid.String()).Error("provider posture: cannot read merchant PSPs")
		}
	}
}

func (r *Runtime) verifyPSPPosture(ctx context.Context, mid merchant.ID, pspID uuid.UUID) {
	svc := r.Merchants
	scope, ok, err := svc.PSPScopeByID(ctx, mid, pspID)
	fields := log.Fields{"merchant_id": mid.String(), "psp_id": pspID.String()}
	if err != nil || !ok {
		log.WithContext(ctx).WithError(err).WithFields(fields).Error("provider posture: PSP unavailable")
		return
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
			return
		}
		if client.LoopbackFixture && client.TestMode {
			return
		}
		if r.NMIPostureV5BaseURL != "" {
			client.V5BaseURL = r.NMIPostureV5BaseURL
		}
		status, check = client.VerifyPosture(ctx), client.PostureCheck()
	case string(models.RailStripe):
		secret, err := pspSecret(ctx, svc, mid, scope, "secret_key")
		if err != nil {
			log.WithContext(ctx).WithError(err).WithFields(fields).Error("provider posture: Stripe credentials unavailable; PSP disarmed")
			return
		}
		status, check = r.StripeClients.VerifyPosture(ctx, secret, scope.AccountID), r.StripeClients.PostureCheckFor(secret, scope.AccountID)
	case string(models.RailSolana):
		if r.SolanaRPCResolver == nil || r.SolanaRPCResolver.Endpoint != "" {
			return
		}
		client, err := r.SolanaRPCResolver.Resolve(ctx, mid)
		if err != nil || client == nil {
			log.WithContext(ctx).WithError(err).WithFields(fields).Error("provider posture: Solana RPC unavailable; PSP disarmed")
			return
		}
		status, check = client.VerifyPosture(ctx), client.CheckPosture
	case string(models.RailCCBill):
		log.WithContext(ctx).WithFields(fields).Warn("provider posture: CCBill has no authoritative test-mode signal; its mutations are refused under sandbox posture")
		return
	default:
		return
	}
	r.providerPosture.AddPSP(pspID, status.Key, check)
	if status.Armed() {
		log.WithContext(ctx).WithFields(fields).Info("provider posture: credentials verified; PSP armed")
		return
	}
	log.WithContext(ctx).WithError(status.Error()).WithFields(fields).Error("provider posture: credentials not verified; PSP disarmed")
}

// PSPPostureDisarmed reports whether pspID failed its posture verification.
func (r *Runtime) PSPPostureDisarmed(pspID uuid.UUID) bool {
	return r != nil && r.providerPosture.PSPDisarmed(providerposture.Process(), pspID)
}

// providerPostureReady fails while any loaded sandbox PSP is disarmed. Due
// unknown verdicts are re-verified here, so a transient provider outage at
// startup recovers without a restart.
func (r *Runtime) providerPostureReady(ctx context.Context) error {
	disarmed := r.providerPosture.Disarmed(ctx, providerposture.Process())
	if len(disarmed) == 0 {
		return nil
	}
	reasons := make([]string, 0, len(disarmed))
	for _, s := range disarmed {
		reasons = append(reasons, s.Error().Error())
	}
	return fmt.Errorf("%d PSP(s) disarmed: %s", len(disarmed), strings.Join(reasons, "; "))
}

func pspSecret(ctx context.Context, svc *merchants.Service, mid merchant.ID, scope merchants.PSPScope, key string) (string, error) {
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
