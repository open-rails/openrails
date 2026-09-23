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

const providerPostureMerchantBatch = 200

// VerifyProviderPosture verifies, once, every PSP credential set this runtime
// loads under test_mode=sandbox. A PSP that is not proven to simulate money
// stays disarmed (its mutations are refused) and Ready reports it; the
// runtime still starts so reads and other PSPs keep working. Hosts that want
// a hard startup failure check Ready after construction.
func (r *Runtime) VerifyProviderPosture(ctx context.Context) {
	if r == nil || r.Config == nil || !r.Config.IsTestMode() || r.DB == nil || r.Merchants == nil {
		return
	}
	r.providerPostureErr.Store(nil)
	environment := config.ExpectedProviderEnvironment(true)
	verifyMerchant := func(mid merchant.ID) {
		if err := r.DB.RunInMerchantScope(ctx, mid, "provider posture", func(mctx context.Context) error {
			rows, err := r.DB.Gen(mctx).ListPSPsForMerchant(mctx, gen.ListPSPsForMerchantParams{MerchantID: mid.UUID()})
			if err != nil {
				return err
			}
			for _, row := range rows {
				if !row.Archived && row.Environment == environment {
					r.verifyPSPPosture(mctx, mid, row.ID)
				}
			}
			return nil
		}); err != nil {
			log.WithContext(ctx).WithError(err).WithField("merchant_id", mid.String()).Error("provider posture: cannot read merchant PSPs")
		}
	}
	// An embedded runtime loads only its configured merchant's credentials.
	if mid := r.ConfiguredMerchant(); !mid.IsZero() {
		verifyMerchant(mid)
		return
	}
	var after *uuid.UUID
	for {
		ids, err := r.DB.GenDirectory().ListRailArmedMerchants(ctx, gen.ListRailArmedMerchantsParams{
			Rails:           []string{string(models.RailNMI), string(models.RailStripe), string(models.RailSolana), string(models.RailCCBill)},
			MerchantLimit:   providerPostureMerchantBatch,
			AfterMerchantID: after,
		})
		if err != nil {
			failure := fmt.Errorf("list PSP merchants: %w", err)
			r.providerPostureErr.Store(&failure)
			log.WithContext(ctx).WithError(err).Error("provider posture: cannot enumerate PSPs; sandbox mutations verify on first use")
			return
		}
		for _, id := range ids {
			if id == nil {
				continue
			}
			after = id
			verifyMerchant(merchant.ID(*id))
		}
		if len(ids) < providerPostureMerchantBatch {
			return
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
		armer := &railresolve.NMIArmer{Config: r.Config, DB: r.DB, MerchantsFn: func() *merchants.Service { return svc }}
		if gateway := r.Config.SandboxNMIGatewayURL(); gateway != "" {
			armer.Endpoints = railresolve.NMIEndpoints{V5BaseURL: gateway, DirectPostURL: gateway, QueryURL: gateway}
		}
		client, err := armer.NMIClient(ctx, mid, scope)
		if err != nil {
			log.WithContext(ctx).WithError(err).WithFields(fields).Error("provider posture: NMI credentials unavailable; PSP disarmed")
			return
		}
		if client.LoopbackFixture {
			return
		}
		if r.NMIPostureV5BaseURL != "" {
			client.V5BaseURL = r.NMIPostureV5BaseURL
		}
		status, check = client.VerifyPosture(ctx), client.CheckPosture
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
	r.providerPosture.Add(status.Key, check)
	if status.Armed() {
		log.WithContext(ctx).WithFields(fields).Info("provider posture: sandbox credentials verified; PSP armed")
		return
	}
	log.WithContext(ctx).WithError(status.Error()).WithFields(fields).Error("provider posture: sandbox credentials not verified; PSP disarmed")
}

// providerPostureReady fails while any loaded sandbox PSP is disarmed. Due
// unknown verdicts are re-verified here, so a transient provider outage at
// startup recovers without a restart.
func (r *Runtime) providerPostureReady(ctx context.Context) error {
	if failed := r.providerPostureErr.Load(); failed != nil {
		r.VerifyProviderPosture(ctx)
		if failed = r.providerPostureErr.Load(); failed != nil {
			return *failed
		}
	}
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
