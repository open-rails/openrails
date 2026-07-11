package intents

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/pkg/merchant"
)

// fakePlanReader maps base58 PDA -> account bytes (missing = absent).
type fakePlanReader struct {
	accounts map[string][]byte
	err      error
}

func (f *fakePlanReader) GetAccountData(_ context.Context, address solanago.PublicKey) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.accounts[address.String()], nil
}

// fakeSubmitter is the recurring.Submitter for unit tests: a fixed merchant
// key, recording submitted instructions.
type fakeSubmitter struct {
	merchant  solanago.PublicKey
	submitted []solanago.Instruction
	submitErr error
}

func (f *fakeSubmitter) MerchantAddress(context.Context, merchant.ID) (solanago.PublicKey, error) {
	return f.merchant, nil
}

func (f *fakeSubmitter) Submit(_ context.Context, _ merchant.ID, ixs []solanago.Instruction) (solanago.Signature, error) {
	if f.submitErr != nil {
		return solanago.Signature{}, f.submitErr
	}
	f.submitted = append(f.submitted, ixs...)
	return solanago.Signature{}, nil
}

// encodePlanAccount builds a decodable Plan account blob with the given owner
// and status.
func encodePlanAccount(owner solanago.PublicKey, status uint8) []byte {
	blob := make([]byte, subscriptions.PlanAccountSize)
	blob[0] = 1 // plan account discriminator
	copy(blob[1:33], owner.Bytes())
	blob[33] = 1 // bump
	blob[34] = status
	return blob
}

func sunsetIntent(t *testing.T, pda string) gen.OpenrailsRailIntent {
	t.Helper()
	payload, err := json.Marshal(SolanaSunsetPayload{PlanPDA: pda})
	if err != nil {
		t.Fatal(err)
	}
	return gen.OpenrailsRailIntent{
		ID:             uuid.New(),
		MerchantID:     dbtest.TestMerchantID.UUID(),
		Rail:           "solana",
		IntentType:     TypeSolanaSunsetPlan,
		Payload:        payload,
		IdempotencyKey: SolanaSunsetIdempotencyKey(pda),
		Origin:         string(OriginAdmin),
	}
}

type sunsetFixture struct {
	handler   *SolanaSunsetPlanHandler
	reader    *fakePlanReader
	submitter *fakeSubmitter
	merchant  solanago.PublicKey
	pda       string
}

func newSunsetFixture() *sunsetFixture {
	merchant := solanago.NewWallet().PublicKey()
	reader := &fakePlanReader{accounts: map[string][]byte{}}
	submitter := &fakeSubmitter{merchant: merchant}
	return &sunsetFixture{
		handler: &SolanaSunsetPlanHandler{
			Reader:      reader,
			Plans:       recurring.NewPlanServiceWithReader(submitter, nil, "devnet"),
			LoadCatalog: stubCatalog(nil, nil),
			Policy:      DefaultBackoff,
		},
		reader:    reader,
		submitter: submitter,
		merchant:  merchant,
		pda:       solanago.NewWallet().PublicKey().String(),
	}
}

func TestSolanaSunset_ExecutesUpdatePlan(t *testing.T) {
	fx := newSunsetFixture()
	fx.reader.accounts[fx.pda] = encodePlanAccount(fx.merchant, subscriptions.PlanStatusActive)

	out := fx.handler.Execute(context.Background(), sunsetIntent(t, fx.pda))
	if out.Class != OutcomeSucceeded {
		t.Fatalf("expected success, got %s (%s)", out.Class, out.Reason)
	}
	if len(fx.submitter.submitted) != 1 {
		t.Fatalf("expected exactly one update_plan submit, got %d", len(fx.submitter.submitted))
	}
	ix := fx.submitter.submitted[0]
	data, err := ix.Data()
	if err != nil {
		t.Fatal(err)
	}
	if data[0] != 8 {
		t.Errorf("update_plan discriminator must be 8, got %d", data[0])
	}
	if data[1] != subscriptions.PlanStatusSunset {
		t.Errorf("status arg must be sunset (0), got %d", data[1])
	}
	if v, _ := out.Evidence["sunset"].(bool); !v {
		t.Errorf("evidence must record the sunset, got %+v", out.Evidence)
	}
}

func TestSolanaSunset_AlreadySunsetIsSuccessWithoutSubmit(t *testing.T) {
	fx := newSunsetFixture()
	fx.reader.accounts[fx.pda] = encodePlanAccount(fx.merchant, subscriptions.PlanStatusSunset)

	out := fx.handler.Execute(context.Background(), sunsetIntent(t, fx.pda))
	if out.Class != OutcomeSucceeded {
		t.Fatalf("already-sunset = success, got %s (%s)", out.Class, out.Reason)
	}
	if v, _ := out.Evidence["already_sunset"].(bool); !v {
		t.Errorf("evidence must record already_sunset, got %+v", out.Evidence)
	}
	if len(fx.submitter.submitted) != 0 {
		t.Error("no transaction may be submitted for an already-sunset plan")
	}
}

func TestSolanaSunset_AbsentPlanIsSuccess(t *testing.T) {
	fx := newSunsetFixture()
	out := fx.handler.Execute(context.Background(), sunsetIntent(t, fx.pda))
	if out.Class != OutcomeSucceeded {
		t.Fatalf("absent plan = success, got %s (%s)", out.Class, out.Reason)
	}
	if v, _ := out.Evidence["verified_absent"].(bool); !v {
		t.Errorf("evidence must record verified_absent, got %+v", out.Evidence)
	}
}

func TestSolanaSunset_ForeignOwnerIsTerminal(t *testing.T) {
	fx := newSunsetFixture()
	fx.reader.accounts[fx.pda] = encodePlanAccount(solanago.NewWallet().PublicKey(), subscriptions.PlanStatusActive)

	out := fx.handler.Execute(context.Background(), sunsetIntent(t, fx.pda))
	if out.Class != OutcomeTerminal {
		t.Fatalf("a plan owned by a different merchant must fail terminally, got %s (%s)", out.Class, out.Reason)
	}
	if len(fx.submitter.submitted) != 0 {
		t.Error("nothing may be submitted against a foreign plan")
	}
}

func TestSolanaSunset_SubmitFailureIsAmbiguous(t *testing.T) {
	fx := newSunsetFixture()
	fx.reader.accounts[fx.pda] = encodePlanAccount(fx.merchant, subscriptions.PlanStatusActive)
	fx.submitter.submitErr = errors.New("rpc timeout mid-send")

	out := fx.handler.Execute(context.Background(), sunsetIntent(t, fx.pda))
	if out.Class != OutcomeAmbiguous {
		t.Fatalf("an on-chain write that may have landed must be ambiguous, got %s (%s)", out.Class, out.Reason)
	}
}

func TestSolanaSunset_ReadonlyChokeParks(t *testing.T) {
	fx := newSunsetFixture()
	fx.reader.accounts[fx.pda] = encodePlanAccount(fx.merchant, subscriptions.PlanStatusActive)
	fx.submitter.submitErr = solanaint.ErrProviderReadOnly

	out := fx.handler.Execute(context.Background(), sunsetIntent(t, fx.pda))
	if out.Class != OutcomeParked {
		t.Fatalf("readonly choke must park, got %s (%s)", out.Class, out.Reason)
	}
}

func TestSolanaSunset_UnconfiguredParks(t *testing.T) {
	h := &SolanaSunsetPlanHandler{LoadCatalog: stubCatalog(nil, nil), Policy: DefaultBackoff}
	out := h.Execute(context.Background(), sunsetIntent(t, solanago.NewWallet().PublicKey().String()))
	if out.Class != OutcomeParked {
		t.Fatalf("unconfigured solana must park, got %s (%s)", out.Class, out.Reason)
	}
}

func TestSolanaSunset_Verify(t *testing.T) {
	fx := newSunsetFixture()
	intent := sunsetIntent(t, fx.pda)

	fx.reader.accounts[fx.pda] = encodePlanAccount(fx.merchant, subscriptions.PlanStatusActive)
	if out := fx.handler.Verify(context.Background(), intent); out.Class != OutcomeRetryable {
		t.Fatalf("still-active verify = retryable, got %s (%s)", out.Class, out.Reason)
	}
	fx.reader.accounts[fx.pda] = encodePlanAccount(fx.merchant, subscriptions.PlanStatusSunset)
	if out := fx.handler.Verify(context.Background(), intent); out.Class != OutcomeSucceeded {
		t.Fatalf("sunset verify = success, got %s (%s)", out.Class, out.Reason)
	}
	delete(fx.reader.accounts, fx.pda)
	if out := fx.handler.Verify(context.Background(), intent); out.Class != OutcomeSucceeded {
		t.Fatalf("absent verify = success, got %s (%s)", out.Class, out.Reason)
	}
}

// TestSolanaSunset_RelevanceFlipsWhenPlanRejoinsCatalog pins ruling 4 for the
// sunset op: relevant while no purchasable local price references the PDA; a
// price going active again (the plan "rejoined the local catalog") supersedes.
func TestSolanaSunset_RelevanceFlipsWhenPlanRejoinsCatalog(t *testing.T) {
	fx := newSunsetFixture()
	intent := sunsetIntent(t, fx.pda)
	cycle := 720
	mkPrice := func(archived bool) *models.Price {
		return &models.Price{
			ID: uuid.New(), ProductID: uuid.New(), Amount: 2300, Currency: "usd",
			AccessDurationHours: &cycle, AutoRenew: true, Archived: archived,
			PSPLinks: map[string]map[string]string{
				string(models.RailSolana): {models.RailKeyRail: "solana", "plan_pda": fx.pda},
			},
		}
	}

	// Referenced only by an ARCHIVED price: still relevant.
	fx.handler.LoadCatalog = stubCatalog(nil, []*models.Price{mkPrice(true)})
	rel, err := fx.handler.CheckRelevance(context.Background(), intent)
	if err != nil || !rel.Applicable {
		t.Fatalf("archived-only reference: must stay relevant, got %+v err=%v", rel, err)
	}

	// A purchasable price references the PDA again: superseded.
	fx.handler.LoadCatalog = stubCatalog(nil, []*models.Price{mkPrice(false)})
	rel, err = fx.handler.CheckRelevance(context.Background(), intent)
	if err != nil || rel.Applicable {
		t.Fatalf("plan rejoined the catalog: must supersede, got %+v err=%v", rel, err)
	}
}
