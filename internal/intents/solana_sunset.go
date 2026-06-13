package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/pkg/tenant"
)

// TypeSolanaSunsetPlan flips an on-chain Subscriptions-program plan to
// status=sunset via update_plan (#357/#358 phase D) — the program's exact
// archive semantics: new subscribe calls are rejected ("Plan is in sunset
// status") while existing subscriptions keep openrails. Produced by the
// `bootstrap apply --exhaustive` sweep for plans whose local price is no
// longer purchasable.
const TypeSolanaSunsetPlan = "solana_sunset_plan"

// SolanaSunsetIdempotencyKey content-addresses one logical sunset: the intent
// type (provider + operation) plus the plan PDA (the remote object id).
func SolanaSunsetIdempotencyKey(planPDA string) string {
	return TypeSolanaSunsetPlan + ":" + strings.TrimSpace(planPDA)
}

// SolanaSunsetPayload is the stored payload for TypeSolanaSunsetPlan.
type SolanaSunsetPayload struct {
	PlanPDA string `json:"plan_pda"`
	Label   string `json:"label,omitempty"`
}

func decodeSolanaSunsetPayload(intent gen.OpenrailsProviderIntent) (SolanaSunsetPayload, solanago.PublicKey, error) {
	var p SolanaSunsetPayload
	if len(intent.Payload) == 0 {
		return p, solanago.PublicKey{}, errors.New("solana sunset intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, solanago.PublicKey{}, fmt.Errorf("decode solana sunset payload: %w", err)
	}
	pda, err := solanago.PublicKeyFromBase58(strings.TrimSpace(p.PlanPDA))
	if err != nil {
		return p, solanago.PublicKey{}, fmt.Errorf("invalid plan_pda %q: %w", p.PlanPDA, err)
	}
	return p, pda, nil
}

// planAccountReader is the read surface the handler needs (verify-then-execute
// + Verify). Satisfied by *solanaint.RPCClient.
type planAccountReader interface {
	GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error)
}

// planSunsetter is the write surface (interface for unit tests). Satisfied by
// *recurring.PlanService.
type planSunsetter interface {
	SunsetPlan(ctx context.Context, tenantID tenant.ID, planPDA solanago.PublicKey, current *subscriptions.PlanAccount) (string, error)
}

// SolanaSunsetPlanHandler implements verify-then-execute sunsetting:
//
//   - relevance: the sunset applies while NO purchasable local price references
//     the plan PDA. A price referencing it going active again (the plan
//     "rejoined the local catalog") supersedes the intent.
//   - execute: read the plan account first — absent or already sunset IS
//     success with evidence; else update_plan(status=sunset) signed by the
//     tenant's merchant key. A submit error is ambiguous (the verifier
//     re-reads the account's status).
//   - unconfigured Solana stack (no RPC / no plan service) parks the intent
//     pending with the reason recorded, never failed.
type SolanaSunsetPlanHandler struct {
	Reader      planAccountReader
	Plans       planSunsetter
	LoadCatalog catalogRowsLoader
	Policy      BackoffPolicy
}

// NewSolanaSunsetPlanHandler wires the handler from runtime dependencies. rpc
// and plans may be nil (Solana unconfigured) — intents then park until the
// deployment grows the capability.
func NewSolanaSunsetPlanHandler(d *db.DB, plans *recurring.PlanService, rpc *solanaint.RPCClient, _ clockwork.Clock) *SolanaSunsetPlanHandler {
	h := &SolanaSunsetPlanHandler{
		LoadCatalog: dbCatalogRowsLoader(d),
		Policy:      DefaultBackoff,
	}
	// Guard the typed-nil-in-interface trap: only assign non-nil concretes.
	if rpc != nil {
		h.Reader = rpc
	}
	if plans != nil {
		h.Plans = plans
	}
	return h
}

func (h *SolanaSunsetPlanHandler) Type() string { return TypeSolanaSunsetPlan }
func (h *SolanaSunsetPlanHandler) Backoff(attempts int32) time.Duration {
	return h.Policy.Delay(attempts)
}

// CheckRelevance: superseded when a purchasable local price references the
// plan PDA again — sunsetting a plan the catalog actively sells would be
// wrong. Local reads only.
func (h *SolanaSunsetPlanHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsProviderIntent) (Relevance, error) {
	p, _, err := decodeSolanaSunsetPayload(intent)
	if err != nil {
		return SupersededBy("unusable solana sunset intent: " + err.Error()), nil
	}
	_, prices, err := h.LoadCatalog(ctx)
	if err != nil {
		return Relevance{}, err
	}
	pda := strings.TrimSpace(p.PlanPDA)
	for _, pr := range prices {
		if !pr.IsPurchasable() {
			continue
		}
		cfg := pr.Processors[string(models.ProcessorSolana)]
		if cfg != nil && strings.TrimSpace(cfg["plan_pda"]) == pda {
			return SupersededBy("a purchasable local price references this plan again; sunsetting it would be wrong"), nil
		}
	}
	return StillRelevant(), nil
}

// readPlan fetches and decodes the plan account. found=false means the account
// is gone from chain.
func (h *SolanaSunsetPlanHandler) readPlan(ctx context.Context, pda solanago.PublicKey) (*subscriptions.PlanAccount, bool, error) {
	data, err := h.Reader.GetAccountData(ctx, pda)
	if err != nil {
		return nil, false, err
	}
	if len(data) == 0 {
		return nil, false, nil
	}
	acct, err := subscriptions.DecodePlanAccount(data)
	if err != nil {
		return nil, false, fmt.Errorf("decode plan account: %w", err)
	}
	return acct, true, nil
}

func (h *SolanaSunsetPlanHandler) Execute(ctx context.Context, intent gen.OpenrailsProviderIntent) Outcome {
	if h.Reader == nil || h.Plans == nil {
		return Parked("solana recurring is not configured (rpc/plan service unavailable)")
	}
	p, pda, err := decodeSolanaSunsetPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}

	// Verify-then-execute: absent or already sunset = success.
	acct, found, err := h.readPlan(ctx, pda)
	if err != nil {
		return Retryable("provider read before sunset failed: " + err.Error())
	}
	if !found {
		return Succeeded(map[string]any{"verified_absent": true, "plan_pda": p.PlanPDA})
	}
	if acct.Status == subscriptions.PlanStatusSunset {
		return Succeeded(map[string]any{"already_sunset": true, "plan_pda": p.PlanPDA})
	}

	sig, err := h.Plans.SunsetPlan(ctx, tenant.ID(intent.TenantID), pda, acct)
	if err != nil {
		if errors.Is(err, solanaint.ErrProviderReadOnly) {
			return Parked("solana provider writes blocked (mode=readonly)")
		}
		if errors.Is(err, recurring.ErrPlanSunsetNotOwned) {
			// Not our plan: re-sending can never succeed.
			return Terminal(err.Error())
		}
		// The transaction may have been submitted; never blind-retry an
		// on-chain write — the verifier re-reads the account status.
		return Ambiguous("sunset submit outcome unknown: " + err.Error())
	}
	return Succeeded(map[string]any{"sunset": true, "plan_pda": p.PlanPDA, "signature": sig})
}

// Verify resolves an ambiguous sunset by reading the plan account back:
// absent/sunset means done (whenever it landed); still active means it
// definitely has not happened and the executor may retry.
func (h *SolanaSunsetPlanHandler) Verify(ctx context.Context, intent gen.OpenrailsProviderIntent) Outcome {
	if h.Reader == nil {
		return Ambiguous("solana rpc not configured; cannot verify")
	}
	p, pda, err := decodeSolanaSunsetPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	acct, found, err := h.readPlan(ctx, pda)
	if err != nil {
		return Ambiguous("provider read failed: " + err.Error())
	}
	if !found {
		return Succeeded(map[string]any{"verified_absent": true, "plan_pda": p.PlanPDA})
	}
	if acct.Status == subscriptions.PlanStatusSunset {
		return Succeeded(map[string]any{"verified_sunset": true, "plan_pda": p.PlanPDA})
	}
	return Retryable("plan still active on-chain; sunset verified not executed")
}
