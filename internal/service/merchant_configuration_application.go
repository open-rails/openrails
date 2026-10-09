package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

func (s *Service) merchantConfigurationState(ctx context.Context) (*billing.MerchantConfigurationState, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	directory, err := s.rt.DB.Gen(ctx).GetMerchantConfigurationDirectory(ctx, mid.UUID())
	if err != nil {
		return nil, err
	}
	settings, err := s.GetMerchantSettings(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(settings.BillingPolicies, func(i, j int) bool { return settings.BillingPolicies[i].Name < settings.BillingPolicies[j].Name })
	sort.Slice(settings.BillingPolicyBindings, func(i, j int) bool {
		a, b := settings.BillingPolicyBindings[i], settings.BillingPolicyBindings[j]
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		return a.PolicyName < b.PolicyName
	})
	state := &billing.MerchantConfigurationState{DisplayName: directory.DisplayName, APIHost: directory.ApiHost, Settings: settings}
	body, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(body)
	state.Revision = hex.EncodeToString(digest[:])
	return state, nil
}

func (s *Service) GetMerchantConfigurationState(ctx context.Context) (state *billing.MerchantConfigurationState, err error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	err = s.rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		scoped := &Service{rt: &app.Runtime{DB: s.rt.DB.NewWithPgxTx(tx)}}
		if _, err := scoped.rt.DB.Gen(ctx).ReadMerchantSettingsLock(ctx, mid.UUID()); err != nil {
			return err
		}
		state, err = scoped.merchantConfigurationState(ctx)
		return err
	})
	return state, err
}

func merchantApplicationDigest(params billing.UpdateMerchantConfigurationParams) ([32]byte, error) {
	// JSON omitempty otherwise erases the distinction between an omitted list
	// and an explicitly empty list that clears declarative policy.
	presence := struct{ Policies, Bindings, Windows bool }{}
	if params.Settings != nil {
		presence.Policies = params.Settings.BillingPolicies != nil
		presence.Bindings = params.Settings.BillingPolicyBindings != nil
		presence.Windows = params.Settings.DelegatedInvokerWastedSpendLimits != nil
	}
	body, err := json.Marshal(struct {
		Params   billing.UpdateMerchantConfigurationParams
		Presence any
	}{params, presence})
	return sha256.Sum256(body), err
}

func (s *Service) UpdateMerchantConfiguration(ctx context.Context, params billing.UpdateMerchantConfigurationParams) (receipt *billing.MerchantConfigurationReceipt, err error) {
	params.IdempotencyKey = strings.TrimSpace(params.IdempotencyKey)
	if params.IdempotencyKey == "" || len(params.IdempotencyKey) > 128 {
		return nil, apperr.New(400, "idempotency_key_required", "Idempotency-Key header is required (at most 128 bytes)")
	}
	if params.ExpectedRevision == nil || strings.TrimSpace(*params.ExpectedRevision) == "" {
		return nil, apperr.Invalidf("expected_revision is required").WithParam("expected_revision")
	}
	if params.DisplayName != nil && strings.TrimSpace(*params.DisplayName) == "" {
		return nil, apperr.Invalidf("display_name must not be empty")
	}
	digest, err := merchantApplicationDigest(params)
	if err != nil {
		return nil, err
	}
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	err = s.rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		database := s.rt.DB.NewWithPgxTx(tx)
		scoped := &Service{rt: &app.Runtime{DB: database}}
		q := database.Gen(ctx)
		if _, err := q.LockMerchantSettings(ctx, mid.UUID()); err != nil {
			return err
		}
		previous, err := q.GetMerchantConfigurationApplication(ctx, gen.GetMerchantConfigurationApplicationParams{MerchantID: mid.UUID(), ApplicationID: params.IdempotencyKey})
		if err == nil {
			if !bytes.Equal(previous.RequestSha256, digest[:]) {
				return apperr.New(422, billing.CodeIdempotencyKeyReused, "Idempotency-Key was already committed with different content")
			}
			receipt = &billing.MerchantConfigurationReceipt{}
			if err := json.Unmarshal(previous.Result, receipt); err != nil {
				return err
			}
			receipt.Replayed = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		current, err := scoped.merchantConfigurationState(ctx)
		if err != nil {
			return err
		}
		if current.Revision != *params.ExpectedRevision {
			return apperr.New(409, "merchant_configuration_revision_conflict", "merchant configuration changed; read its current revision before applying")
		}
		if params.Settings != nil {
			if err := scoped.SetMerchantSettings(ctx, mergeMerchantSettings(current.Settings, *params.Settings)); err != nil {
				return err
			}
		}
		if params.DisplayName != nil {
			if err := q.SetMerchantConfigurationDisplayName(ctx, gen.SetMerchantConfigurationDisplayNameParams{MerchantID: mid.UUID(), DisplayName: strings.TrimSpace(*params.DisplayName)}); err != nil {
				return err
			}
		}
		current, err = scoped.merchantConfigurationState(ctx)
		if err != nil {
			return err
		}
		receipt = &billing.MerchantConfigurationReceipt{Revision: current.Revision}
		body, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		return q.InsertMerchantConfigurationApplication(ctx, gen.InsertMerchantConfigurationApplicationParams{MerchantID: mid.UUID(), ApplicationID: params.IdempotencyKey, RequestSha256: digest[:], Result: body})
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01") {
			return nil, apperr.New(409, "merchant_configuration_revision_conflict", "concurrent merchant configuration change; retry after reading its revision")
		}
		return nil, err
	}
	return receipt, nil
}

func mergeMerchantSettings(current, patch billing.MerchantSettings) billing.MerchantSettings {
	if patch.Profile != nil {
		p := billing.MerchantProfile{}
		if current.Profile != nil {
			p = *current.Profile
		}
		if patch.Profile.DisplayName != "" {
			p.DisplayName = patch.Profile.DisplayName
		}
		if patch.Profile.LogoURL != "" {
			p.LogoURL = patch.Profile.LogoURL
		}
		if patch.Profile.FromEmail != "" {
			p.FromEmail = patch.Profile.FromEmail
		}
		if patch.Profile.SupportURL != "" {
			p.SupportURL = patch.Profile.SupportURL
		}
		if patch.Profile.SignupURL != "" {
			p.SignupURL = patch.Profile.SignupURL
		}
		current.Profile = &p
	}
	if patch.InvoiceCollectionThreshold != nil {
		current.InvoiceCollectionThreshold = patch.InvoiceCollectionThreshold
	}
	if patch.InvoiceMonthlyFloor != nil {
		current.InvoiceMonthlyFloor = patch.InvoiceMonthlyFloor
	}
	if patch.InvoiceBillingBoundary != "" {
		current.InvoiceBillingBoundary = patch.InvoiceBillingBoundary
	}
	if patch.AlertEmail != nil {
		current.AlertEmail = patch.AlertEmail
	}
	if patch.RepriceNoticeWindowDays != nil {
		current.RepriceNoticeWindowDays = patch.RepriceNoticeWindowDays
	}
	if patch.RenewalReceiptMinIntervalHours != nil {
		current.RenewalReceiptMinIntervalHours = patch.RenewalReceiptMinIntervalHours
	}
	if patch.ProviderRefundAccess != nil {
		current.ProviderRefundAccess = patch.ProviderRefundAccess
	}
	if patch.ArrearsGraceDays != nil {
		current.ArrearsGraceDays = patch.ArrearsGraceDays
	}
	if patch.ArrearsDelinquencyFloor != nil {
		current.ArrearsDelinquencyFloor = patch.ArrearsDelinquencyFloor
	}
	if patch.CheckoutRouting != nil {
		current.CheckoutRouting = patch.CheckoutRouting
	}
	if patch.DunningPolicy != nil {
		current.DunningPolicy = patch.DunningPolicy
	}
	if patch.BillingPolicies != nil {
		current.BillingPolicies = patch.BillingPolicies
	}
	if patch.BillingPolicyBindings != nil {
		current.BillingPolicyBindings = patch.BillingPolicyBindings
	}
	if patch.DelegatedInvokerWastedSpendLimits != nil {
		current.DelegatedInvokerWastedSpendLimits = patch.DelegatedInvokerWastedSpendLimits
	}
	return current
}
