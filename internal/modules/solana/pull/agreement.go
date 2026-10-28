package pull

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Agreement freezes the complete accepted obligation before its
// transaction is signed. EpisodeAt identifies the due pull; realized service
// dates come from its verified chain receipt, including after a restart.
type Agreement struct {
	Fingerprint           int64                     `json:"plan_fingerprint"`
	Row                   models.SolanaSubscription `json:"row"`
	PriceID               uuid.UUID                 `json:"price_id"`
	ProductID             uuid.UUID                 `json:"product_id"`
	CustomerID            uuid.UUID                 `json:"customer_id"`
	Description           string                    `json:"description"`
	Amount                int64                     `json:"amount,string"`
	Currency              string                    `json:"currency"`
	NativeDecimals        int                       `json:"native_decimals"`
	TokenAmount           uint64                    `json:"token_amount,string"`
	TokenDecimals         int                       `json:"token_decimals"`
	PeriodHours           uint64                    `json:"period_hours,string"`
	AccessDurationHours   *int                      `json:"access_duration_hours"`
	ProviderAccessDefault bool                      `json:"provider_access_default,omitempty"`
	Quantity              *int                      `json:"quantity"`
	RetryAttempts         int                       `json:"retry_attempts"`
	EpisodeAt             time.Time                 `json:"episode_at"`
	PreviousPeriodEnd     *time.Time                `json:"previous_period_end"`
}

func (a *Agreement) validate(in gen.BillingProviderIntent, p Payload) error {
	if a == nil || in.SubscriptionID == nil || a.Row.SubscriptionID != *in.SubscriptionID || a.Row.MerchantID != in.MerchantID || in.PspID == nil || a.Row.PspID != *in.PspID || a.Row.ID != p.RowID || a.Row.SubscriptionPDA != p.SubscriptionPDA || !a.EpisodeAt.Equal(p.NextPullAt) || !a.Row.NextPullAt.Equal(p.NextPullAt) || a.PriceID == uuid.Nil || a.ProductID == uuid.Nil || a.CustomerID == uuid.Nil || a.PreviousPeriodEnd == nil || a.Amount <= 0 || a.TokenAmount == 0 || a.PeriodHours == 0 || a.PeriodHours > 24*366 {
		return errors.New("accepted Solana pull agreement does not own this episode")
	}
	units, ok := moneyutil.LookupCurrency(a.Currency)
	if !ok || a.Currency != units.Code || a.NativeDecimals != units.Decimals {
		return errors.New("accepted Solana pull currency precision is invalid")
	}
	for _, key := range []string{a.Row.SubscriberWallet, a.Row.AuthorityPDA, a.Row.SubscriptionPDA, a.Row.PlanPDA, a.Row.MerchantAddress, a.Row.Mint, a.Row.Treasury} {
		if _, err := solanago.PublicKeyFromBase58(key); err != nil {
			return errors.New("accepted Solana pull account is invalid")
		}
	}
	return nil
}

func (h *Handler) acceptPull(ctx context.Context, row *models.SolanaSubscription, plan resolvedPlan) (*Agreement, error) {
	reader, ok := h.Chain.(solanaint.MintAccountReader)
	if !ok {
		return nil, errors.New("Solana pull requires authoritative mint metadata")
	}
	mint, err := solanago.PublicKeyFromBase58(row.Mint)
	if err != nil {
		return nil, err
	}
	decimals, err := solanaint.ReadMintDecimals(ctx, reader, mint)
	if err != nil {
		return nil, err
	}
	units, ok := moneyutil.LookupCurrency(plan.currency)
	if !ok {
		return nil, fmt.Errorf("unsupported accepted currency %s", plan.currency)
	}
	acceptedRow := *row
	if acceptedRow.PspID != uuid.Nil && acceptedRow.PspID != plan.pspID {
		return nil, errors.New("pull PSP contradicts the accepted subscription")
	}
	acceptedRow.PspID = plan.pspID
	return &Agreement{Fingerprint: plan.fingerprint, Row: acceptedRow, PriceID: plan.priceID, ProductID: plan.productID, CustomerID: plan.customerID,
		RetryAttempts: plan.retryAttempts, Description: plan.description, Amount: plan.fiatAmount, Currency: units.Code, NativeDecimals: units.Decimals,
		TokenAmount: plan.amountBaseUnits, TokenDecimals: decimals, PeriodHours: plan.periodHours,
		AccessDurationHours: plan.accessDurationHours, ProviderAccessDefault: plan.providerAccessDefault, Quantity: plan.quantity, EpisodeAt: row.NextPullAt, PreviousPeriodEnd: plan.periodEnd}, nil
}

func (a *Agreement) plan() resolvedPlan {
	return resolvedPlan{retryAttempts: a.RetryAttempts, amountBaseUnits: a.TokenAmount, periodHours: a.PeriodHours, fingerprint: a.Fingerprint,
		fiatAmount: a.Amount, currency: a.Currency, customerID: a.CustomerID, pspID: a.Row.PspID, policy: models.CollectionPolicyEngine,
		priceID: a.PriceID, productID: a.ProductID, description: a.Description, accessDurationHours: a.AccessDurationHours, providerAccessDefault: a.ProviderAccessDefault, quantity: a.Quantity, periodEnd: a.PreviousPeriodEnd}
}

// errPullEpisodeOver: the subscription no longer owns this pull episode
// (canceled, collected elsewhere, or its mirror moved on); nothing is pulled.
var errPullEpisodeOver = errors.New("Solana pull superseded before submission")

// errPullTermsMoved: the subscription's terms changed after the agreement was
// frozen; the next attempt freezes them again.
var errPullTermsMoved = errors.New("Solana pull terms changed before submission")

// pullEpisodeState says whether sub and its mirror still own the episode at
// nextPullAt, and whether agreement still matches the subscription's terms.
func pullEpisodeState(sub *models.Subscription, current *models.SolanaSubscription, nextPullAt time.Time, agreement *Agreement) error {
	if sub.Status == models.StatusCanceled || sub.CollectionPolicy != models.CollectionPolicyEngine || current.Status != models.SolanaSubscriptionActive || current.NextPullAt.UTC().Unix() != nextPullAt.UTC().Unix() {
		return errPullEpisodeOver
	}
	if sub.PriceID != agreement.PriceID || sub.ProductID != agreement.ProductID || sub.CustomerID != agreement.CustomerID || sub.PspID != agreement.Row.PspID {
		return errPullTermsMoved
	}
	return nil
}

// freezeAgreement freezes what this attempt pulls from the subscription as it
// stands now, a due scheduled change applied, and records it on the claimed
// operation before anything is signed. Only an attempt with nothing signed, or
// whose signed transaction reverted, freezes.
func (h *Handler) freezeAgreement(ctx context.Context, in gen.BillingProviderIntent, p Payload, row *models.SolanaSubscription) (*Agreement, error) {
	plan, err := h.resolvePlan(ctx, row)
	if err != nil {
		return nil, err
	}
	agreement, err := h.acceptPull(ctx, row, plan)
	if err != nil {
		return nil, err
	}
	if err := agreement.validate(in, p); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(agreement)
	if err != nil {
		return nil, err
	}
	err = h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, agreement.Row.SubscriptionID)
		if err != nil {
			return err
		}
		current, err := solanasubs.NewSolanaSubscriptionRepo(d).GetBySubscriptionPDA(ctx, p.SubscriptionPDA)
		if err != nil {
			return err
		}
		if err := pullEpisodeState(sub, current, p.NextPullAt, agreement); err != nil {
			return err
		}
		changed, err := d.Gen(ctx).FreezeSolanaPullAgreement(ctx, gen.FreezeSolanaPullAgreementParams{MerchantID: in.MerchantID, ID: in.ID, Attempts: in.Attempts, Agreement: encoded, Now: h.now()})
		if err != nil {
			return err
		}
		if changed != 1 {
			return errors.New("pull claim changed before its agreement froze")
		}
		return nil
	})
	return agreement, err
}
