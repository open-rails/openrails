// Package custodymigration is or#297 Phase C: the vault-import + token-remap
// mechanism that makes a PSP-vaulted card book survive a de-platforming.
//
// THE SCENARIO. A processor drops the merchant. Cards vaulted AT that processor
// are hostage: OpenRails never held the PAN, and the party that did is the one
// that just terminated the relationship. Cards held by a neutral custodian
// (or#880) are portable — the custodian exports them to any PCI-AoC
// destination. The card book that is stuck is exactly the `custodian='psp'`
// half of openrails.payment_methods.
//
// WHAT IS OPERATIONAL, WHAT IS MECHANISM. Getting the cards out is an
// OPERATIONAL act performed by humans and vendors: the merchant obtains a PCI
// vault export from the processor/acquirer and the custodian ingests it. No
// PAN passes through OpenRails at any point, and this package makes no vendor
// call. What it consumes is the RESULT — a manifest mapping each original vault
// entry to the custodian token that now stands for the same card.
//
// WHAT THIS PACKAGE DOES. Two halves of one operation:
//
//   - IMPORT: land that manifest as declared FACTS (the #737 ImportBilling
//     shape — declared input, whole-body validation, per-row outcomes,
//     idempotent, durable), creating custodian-held instrument records where
//     the operator declares a customer for a card our book never had.
//
//   - REMAP: per instrument, an ATOMIC custody flip from PSP-vaulted to
//     custodian-held on the SAME payment_method_id. That identity is the whole
//     point of the seam or#879/or#880 built: subscriptions reference the
//     instrument, and the charge transport is a function of the instrument's
//     custody, so nothing about a subscription changes and the post-remap
//     charge goes through the existing custodian proxy path with no further
//     code.
//
// FOUR PROPERTIES THE FLIP MUST HAVE (they are the design, not decoration):
//
//  1. The old PSP vault handle is never lost. It stays on the payment_methods
//     row (rail_customer_ref) AND is copied into openrails.custody_migrations.
//  2. Subscriptions are untouched. Same payment_method_id, no lifecycle write.
//  3. Reversible IN RECORD, not in custody. Every field needed to re-point an
//     instrument back at its PSP vault is recorded. That does not restore the
//     card: the processor may have deleted the vault entry, or terminated the
//     merchant — which is the event this exists for.
//  4. No charge may straddle the flip. An instrument whose subscription has a
//     charge intent in_flight or unknown_needs_verify is REFUSED for this run,
//     not failed: both states clear on their own, so the operator re-runs.
//
// DRY RUN FIRST. Options.Apply defaults to false: a plan performs every read
// and every refusal check and writes nothing, so the operator sees the counts
// by outcome before a single instrument moves.
package custodymigration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmiproxy"
	"github.com/open-rails/openrails/pkg/merchant"
)

// PSPRef names a PSP by the operator-declared natural identity (#592: there is
// no runtime whoami; account_id is a declared label). Environment "" = live.
type PSPRef struct {
	Rail        string `json:"rail"`
	Environment string `json:"environment,omitempty"`
	AccountID   string `json:"account_id"`
}

func (r PSPRef) String() string {
	env := strings.TrimSpace(r.Environment)
	if env == "" {
		env = "live"
	}
	return fmt.Sprintf("%s/%s/%s", strings.ToLower(strings.TrimSpace(r.Rail)), env, strings.TrimSpace(r.AccountID))
}

func (r PSPRef) isZero() bool {
	return strings.TrimSpace(r.Rail) == "" && strings.TrimSpace(r.AccountID) == ""
}

// ImportedToken is one line of the custodian's post-ingest manifest: the
// original vault entry, and the custodian token that now stands for that card.
type ImportedToken struct {
	// SourceRailCustomerRef is the customer-scope handle at the PSP — for NMI
	// the customer_vault_id, which under one-vault-per-card minting (#682) is
	// the card's address.
	SourceRailCustomerRef string `json:"source_rail_customer_ref"`
	// SourceRailMethodRef narrows within a shared (imported multi-card) vault;
	// empty for the one-vault-per-card default.
	SourceRailMethodRef string `json:"source_rail_method_ref,omitempty"`

	// Token is the custodian's token id — payment_methods.rail_method_ref
	// after the flip.
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint,omitempty"`
	LastFour    string `json:"last_four,omitempty"`
	CardType    string `json:"card_type,omitempty"`
	ExpiryDate  string `json:"expiry_date,omitempty"`

	NetworkTokenID     string `json:"network_token_id,omitempty"`
	NetworkTokenStatus string `json:"network_token_status,omitempty"`
	NetworkTokenPAR    string `json:"network_token_par,omitempty"`

	// Customer, when set, lets the import CREATE a custodian-held instrument
	// for a card the export carried but our book has no row for. Absent, an
	// unmatched line is reported as such and nothing is invented (#651: no
	// silent fabrication).
	Customer *uuid.UUID `json:"customer,omitempty"`
}

// VaultExport is the declared body: one custodian ingest of one PSP vault
// export.
type VaultExport struct {
	// ExportedAt is the evidence horizon — when the custodian's ingest was
	// true. Required, same doctrine as billingimport.AsOf.
	ExportedAt time.Time `json:"exported_at"`

	// SourceRail is the rail the cards were vaulted on (nmi). The rail does not
	// change: custody moves, the gateway kind does not (or#879).
	SourceRail string `json:"source_rail"`

	// Custodian is the merchant's declared custodian KEY (custodians.key) that
	// ingested the export.
	Custodian string `json:"custodian"`

	// PSP, when set, re-points the remapped instruments at a DIFFERENT gateway
	// account — the deplatforming case: same card file, new processor. It must
	// reference the declared custodian. Absent, instruments keep the PSP they
	// already carry, which only works if that PSP already references the
	// custodian.
	PSP PSPRef `json:"psp,omitempty"`

	// ExpectedTokens is the typed confirmation of len(Tokens) (the or#858
	// shape): a truncated manifest cannot slip through as a short run.
	ExpectedTokens *int `json:"expected_tokens,omitempty"`

	Tokens []ImportedToken `json:"tokens"`
}

// Options configures Migrate. Merchant scoping mirrors billingimport:
// MerchantID when the caller already resolved it, else MerchantSlug.
type Options struct {
	Config       *config.Config
	PGXPool      *pgxpool.Pool
	MerchantSlug string
	MerchantID   merchant.ID
	Export       VaultExport

	// Apply=false (the default) is a DRY RUN: every read and every refusal
	// check runs, nothing is written. Plan first — the destructive-tooling
	// doctrine, and custody is not reversible in custody.
	Apply bool

	// BatchID stamps the run. Zero = generated.
	BatchID uuid.UUID
}

// Outcome is the per-token verdict vocabulary.
type Outcome string

const (
	// OutcomeRemapped: an existing instrument changed custody. Same
	// payment_method_id, subscriptions untouched.
	OutcomeRemapped Outcome = "remapped"
	// OutcomeCreated: the export carried a card with no local instrument and
	// the operator declared its customer, so a custodian-held row was minted.
	OutcomeCreated Outcome = "created"
	// OutcomeAlreadyMigrated: this instrument already holds this custodian
	// token — an idempotent re-run of the same manifest.
	OutcomeAlreadyMigrated Outcome = "already_migrated"
	// OutcomeUnmatched: no local instrument for that source vault handle and
	// no declared customer. Reported, never invented.
	OutcomeUnmatched Outcome = "unmatched"
	// OutcomeBlocked: a refusal. Reason says which.
	OutcomeBlocked Outcome = "blocked"
)

// Block reasons (RowResult.Reason when Outcome is OutcomeBlocked).
const (
	// ReasonChargeInFlight: the instrument's subscription has a charge intent
	// mid-attempt (in_flight, or sent-and-unverified). No charge may straddle
	// the flip. Transient by construction — re-run.
	ReasonChargeInFlight = "charge_in_flight"
	// ReasonTokenConflict: another instrument already holds this custodian
	// token. Two instruments pointing at one card is never right.
	ReasonTokenConflict = "token_conflict"
	// ReasonCustodyConflict: the instrument is already custodian-held under a
	// DIFFERENT token. A second flip needs an operator decision, not a guess.
	ReasonCustodyConflict = "custody_conflict"
	// ReasonMissingTargetPSP: a card with no local row can only be CREATED
	// against the export's declared target PSP — provenance is total, so an
	// instrument cannot exist unattributed (or#893).
	ReasonMissingTargetPSP = "missing_target_psp"
	// ReasonMissingToken: the manifest line carries no custodian token.
	ReasonMissingToken = "missing_token"
	// ReasonMissingSourceRef: the line names no source vault handle and no
	// customer, so it identifies nothing.
	ReasonMissingSourceRef = "missing_source_ref"
	// ReasonDuplicateLine: the manifest names the same source handle or the
	// same token twice.
	ReasonDuplicateLine = "duplicate_line"
	// ReasonRailMismatch: the resolved instrument is on a different rail than
	// the export declares.
	ReasonRailMismatch = "rail_mismatch"
	// ReasonMissingCustomer: a create was implied but no customer was declared.
	ReasonMissingCustomer = "missing_customer"
)

// RowResult is one manifest line's verdict.
type RowResult struct {
	Token                 string     `json:"token"`
	SourceRailCustomerRef string     `json:"source_rail_customer_ref"`
	PaymentMethodID       *uuid.UUID `json:"payment_method_id,omitempty"`
	Outcome               Outcome    `json:"outcome"`
	Reason                string     `json:"reason,omitempty"`
	// FromRailCustomerRef/FromRailMethodRef are the handles the instrument is
	// leaving behind — echoed into the report so a plan shows exactly what is
	// about to be superseded.
	FromRailCustomerRef string `json:"from_rail_customer_ref,omitempty"`
	FromRailMethodRef   string `json:"from_rail_method_ref,omitempty"`
}

// Result is the run report. Counts is the dry-run's headline.
type Result struct {
	BatchID   uuid.UUID       `json:"batch_id"`
	Applied   bool            `json:"applied"`
	Custodian string          `json:"custodian"`
	Counts    map[Outcome]int `json:"counts"`
	Rows      []RowResult     `json:"rows"`
}

// Migrate plans (Apply=false) or applies (Apply=true) one vault export.
//
// Whole-body problems — a missing horizon, an undeclared custodian, a PSP that
// does not reference it, a token count that disagrees with the manifest —
// refuse the ENTIRE run with an error. They are declaration errors, and a
// half-applied custody migration is the one outcome worth avoiding above all.
// Per-line problems are reported as outcomes and never abort the run.
//
// Each applied flip is its own transaction: one refused instrument must not
// roll back the ones that already moved.
func Migrate(ctx context.Context, opts Options) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	res := Result{BatchID: opts.BatchID, Applied: opts.Apply, Counts: map[Outcome]int{}}
	if res.BatchID == uuid.Nil {
		res.BatchID = uuid.New()
	}

	exp := opts.Export
	if exp.ExportedAt.IsZero() {
		return res, errors.New("custody migration: Export.ExportedAt (the custodian ingest horizon) is required")
	}
	sourceRail := strings.ToLower(strings.TrimSpace(exp.SourceRail))
	if sourceRail == "" {
		return res, errors.New("custody migration: Export.SourceRail is required")
	}
	custodianKey := strings.TrimSpace(exp.Custodian)
	if custodianKey == "" {
		return res, errors.New("custody migration: Export.Custodian (the declared custodian key) is required")
	}
	if exp.ExpectedTokens != nil && *exp.ExpectedTokens != len(exp.Tokens) {
		return res, fmt.Errorf("custody migration: Export.ExpectedTokens is %d but the manifest carries %d tokens — refusing a run whose declaration and body disagree",
			*exp.ExpectedTokens, len(exp.Tokens))
	}
	if len(exp.Tokens) == 0 {
		return res, nil
	}

	database, err := openDB(opts.Config, opts.PGXPool)
	if err != nil {
		return res, err
	}
	defer database.Close()

	merchantID := opts.MerchantID
	if merchantID.IsZero() {
		merchantID, err = db.ResolveMerchantSlug(ctx, database.Pool(), strings.TrimSpace(opts.MerchantSlug))
		if err != nil {
			return res, fmt.Errorf("custody migration: resolve merchant %q: %w", opts.MerchantSlug, err)
		}
	}
	ctx = merchant.WithID(ctx, merchantID)

	// Resolve the declared targets ONCE, before any row moves: an undeclared
	// custodian or a PSP that charges through someone else's vault is a
	// declaration error, and finding it on row 4000 is worthless.
	var (
		custodian gen.OpenrailsCustodian
		targetPSP *gen.OpenrailsPsp
	)
	err = database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := database.Gen(ctx)
		custodian, err = q.GetCustodianByKey(ctx, gen.GetCustodianByKeyParams{
			MerchantID: merchantID.UUID(), Key: custodianKey,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("custody migration: merchant declares no custodian %q — declare merchants.<slug>.custodians.%s.<kind> first", custodianKey, custodianKey)
			}
			return fmt.Errorf("custody migration: resolve custodian %q: %w", custodianKey, err)
		}
		if custodian.Archived {
			return fmt.Errorf("custody migration: custodian %q is archived — an archived custodian drains existing instruments, it does not receive new ones", custodianKey)
		}
		if custodians.Normalize(custodian.Kind) != models.CustodianBasisTheory {
			return fmt.Errorf("custody migration: custodian %q is kind %q; only %q holds cards today", custodianKey, custodian.Kind, models.CustodianBasisTheory)
		}
		if exp.PSP.isZero() {
			return nil
		}
		psp, perr := q.GetPSPByIdentity(ctx, gen.GetPSPByIdentityParams{
			MerchantID:  merchantID.UUID(),
			Rail:        exp.PSP.Rail,
			Environment: nilIfEmpty(exp.PSP.Environment),
			AccountID:   strings.TrimSpace(exp.PSP.AccountID),
		})
		if perr != nil {
			if errors.Is(perr, pgx.ErrNoRows) {
				return fmt.Errorf("custody migration: merchant declares no PSP %s", exp.PSP)
			}
			return fmt.Errorf("custody migration: resolve PSP %s: %w", exp.PSP, perr)
		}
		if psp.CustodianID == nil || *psp.CustodianID != custodian.ID {
			return fmt.Errorf("custody migration: PSP %s does not reference custodian %q — a remapped instrument would be charged by a gateway that cannot detokenize it", exp.PSP, custodianKey)
		}
		if psp.Archived {
			return fmt.Errorf("custody migration: PSP %s is archived — it drains existing obligations, it does not take on a migrated book", exp.PSP)
		}
		targetPSP = &psp
		return nil
	})
	if err != nil {
		return res, err
	}
	res.Custodian = custodianKey

	// Duplicate detection is a property of the MANIFEST, so it is decided here
	// rather than per row: the same card cannot be claimed twice in one run.
	seenSource := map[string]int{}
	seenToken := map[string]int{}
	for i, tk := range exp.Tokens {
		if src := sourceKey(tk); src != "" {
			seenSource[src]++
		}
		if t := strings.TrimSpace(tk.Token); t != "" {
			seenToken[t]++
		}
		_ = i
	}

	plan := &planner{
		db:         database,
		merchantID: merchantID,
		batchID:    res.BatchID,
		sourceRail: sourceRail,
		custodian:  custodian,
		targetPSP:  targetPSP,
		exportedAt: exp.ExportedAt.UTC(),
		apply:      opts.Apply,
		seenSource: seenSource,
		seenToken:  seenToken,
	}

	res.Rows = make([]RowResult, 0, len(exp.Tokens))
	for _, tk := range exp.Tokens {
		row, rerr := plan.one(ctx, tk)
		if rerr != nil {
			return res, rerr
		}
		res.Rows = append(res.Rows, row)
		res.Counts[row.Outcome]++
	}
	return res, nil
}

type planner struct {
	db         *db.DB
	merchantID merchant.ID
	batchID    uuid.UUID
	sourceRail string
	custodian  gen.OpenrailsCustodian
	targetPSP  *gen.OpenrailsPsp
	exportedAt time.Time
	apply      bool
	seenSource map[string]int
	seenToken  map[string]int
}

func sourceKey(tk ImportedToken) string {
	c := strings.TrimSpace(tk.SourceRailCustomerRef)
	m := strings.TrimSpace(tk.SourceRailMethodRef)
	if c == "" && m == "" {
		return ""
	}
	return c + "\x1f" + m
}

// one decides and (when applying) performs a single manifest line.
func (p *planner) one(ctx context.Context, tk ImportedToken) (RowResult, error) {
	token := strings.TrimSpace(tk.Token)
	src := strings.TrimSpace(tk.SourceRailCustomerRef)
	out := RowResult{Token: token, SourceRailCustomerRef: src}

	if token == "" {
		out.Outcome, out.Reason = OutcomeBlocked, ReasonMissingToken
		return out, nil
	}
	if src == "" && strings.TrimSpace(tk.SourceRailMethodRef) == "" && tk.Customer == nil {
		out.Outcome, out.Reason = OutcomeBlocked, ReasonMissingSourceRef
		return out, nil
	}
	if p.seenToken[token] > 1 || (sourceKey(tk) != "" && p.seenSource[sourceKey(tk)] > 1) {
		out.Outcome, out.Reason = OutcomeBlocked, ReasonDuplicateLine
		return out, nil
	}

	// READ PHASE — identical in plan and apply, so the plan's verdict is the
	// verdict. The apply path re-decides under a row lock; that is the only
	// difference, and it can only turn a would-be flip into a refusal.
	var (
		existing    *gen.OpenrailsPaymentMethod
		tokenHolder *gen.OpenrailsPaymentMethod
	)
	err := p.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := p.db.Gen(ctx)
		if holder, herr := q.GetPaymentMethodForCustodianToken(ctx, gen.GetPaymentMethodForCustodianTokenParams{
			MerchantID:    p.merchantID.UUID(),
			Custodian:     p.custodianKind(),
			RailMethodRef: token,
		}); herr == nil {
			tokenHolder = &holder
		} else if !errors.Is(herr, pgx.ErrNoRows) {
			return fmt.Errorf("look up holder of custodian token %s: %w", token, herr)
		}
		if src == "" && strings.TrimSpace(tk.SourceRailMethodRef) == "" {
			return nil
		}
		row, rerr := q.GetPaymentMethodByRailInstrument(ctx, gen.GetPaymentMethodByRailInstrumentParams{
			MerchantID:      p.merchantID.UUID(),
			Rail:            p.sourceRail,
			RailCustomerRef: src,
			RailMethodRef:   strings.TrimSpace(tk.SourceRailMethodRef),
		})
		if rerr != nil {
			if errors.Is(rerr, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("resolve instrument for vault handle %s: %w", src, rerr)
		}
		existing = &row
		return nil
	})
	if err != nil {
		return out, err
	}

	if existing == nil {
		// Nothing local claims that vault handle. Either the operator declared
		// the customer (mint a custodian-held instrument) or we report it.
		if tokenHolder != nil {
			// The token is already ours under some other instrument: that IS
			// the idempotent re-run of a `created` line.
			out.PaymentMethodID = &tokenHolder.ID
			out.Outcome = OutcomeAlreadyMigrated
			return out, nil
		}
		if tk.Customer == nil {
			out.Outcome, out.Reason = OutcomeUnmatched, ""
			return out, nil
		}
		if *tk.Customer == uuid.Nil {
			out.Outcome, out.Reason = OutcomeBlocked, ReasonMissingCustomer
			return out, nil
		}
		if p.targetPSP == nil {
			out.Outcome, out.Reason = OutcomeBlocked, ReasonMissingTargetPSP
			return out, nil
		}
		return p.create(ctx, tk, out)
	}

	out.PaymentMethodID = &existing.ID
	out.FromRailCustomerRef = existing.RailCustomerRef
	out.FromRailMethodRef = existing.RailMethodRef

	if !strings.EqualFold(existing.Rail, p.sourceRail) {
		out.Outcome, out.Reason = OutcomeBlocked, ReasonRailMismatch
		return out, nil
	}
	if tokenHolder != nil && tokenHolder.ID != existing.ID {
		out.Outcome, out.Reason = OutcomeBlocked, ReasonTokenConflict
		return out, nil
	}
	if existing.Custodian == p.custodianKind() {
		if existing.RailMethodRef == token {
			out.Outcome = OutcomeAlreadyMigrated
			return out, nil
		}
		out.Outcome, out.Reason = OutcomeBlocked, ReasonCustodyConflict
		return out, nil
	}
	return p.remap(ctx, tk, existing, out)
}

func (p *planner) custodianKind() string { return custodians.Normalize(p.custodian.Kind) }

func (p *planner) targetPSPID() *uuid.UUID {
	if p.targetPSP == nil {
		return nil
	}
	id := p.targetPSP.ID
	return &id
}

func openDB(cfg *config.Config, pool *pgxpool.Pool) (*db.DB, error) {
	if pool != nil {
		schema := config.DefaultSchema
		if cfg != nil && cfg.DB != nil {
			schema = cfg.DB.SchemaName()
		}
		return db.NewWithPGXPool(pool, schema)
	}
	if cfg == nil || cfg.DB == nil {
		return nil, errors.New("custody migration: config database is required")
	}
	database, err := db.NewDB(cfg.DB)
	if err != nil {
		return nil, fmt.Errorf("custody migration: open postgres: %w", err)
	}
	return database, nil
}

func nilIfEmpty(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	v := strings.TrimSpace(s)
	return &v
}

// SortedCounts renders Counts in a stable operator-facing order.
func (r Result) SortedCounts() []string {
	keys := make([]string, 0, len(r.Counts))
	for k := range r.Counts {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s=%d", k, r.Counts[Outcome(k)]))
	}
	return out
}

// chargeViaFor picks the post-remap charge transport. Network-token charging is
// gated off on NMI gateways (#795), so an imported instrument charges the
// detokenized PAN through the custodian proxy regardless of whether the export
// carried a network token — the NT is recorded, never made load-bearing.
func chargeViaFor(_ ImportedToken) string { return nmiproxy.ViaPANProxy }
