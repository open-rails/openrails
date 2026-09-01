//go:build integration

package integrationharness

// Issue #620 — Solana money-movement proof.
//
// Acceptance is a DATA proof, not a funded on-chain transfer:
//
//	(a) Solana inputs/outputs carry ONLY the payment/plan identifiers needed for
//	    money movement (amount, mint, recipient, a reference id) — NOT catalog,
//	    product, entitlement, usage, or invoice metadata.
//	(b) The OpenRails Postgres DB remains the source of truth for product
//	    metadata, granted benefits, and invoice/payment state.
//
// Opt-in: the test SKIPS when OPENRAILS_TEST_SOLANA_PRIVATE_KEY is unset, and
// FAILS LOUD when the key is set but the metadata/construction path errors. The
// real on-chain USDC transfer leg runs only when the devnet wallet is funded; a
// confirmation failure there is logged, never fatal (the metadata proof is the
// acceptance, and devnet is a flaky external dependency).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	ata "github.com/gagliardetto/solana-go/programs/associated-token-account"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/money"
	solanamod "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/catalog"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

const (
	solanaPrivateKeyEnv = "OPENRAILS_TEST_SOLANA_PRIVATE_KEY"
	// The base58 private key in .env derives this wallet (the documented merchant
	// crank/recipient address). Asserted so a swapped key fails loud.
	expectedSolanaWallet = "4roUXkiChfoyn4y9KJcEz8g4VNtCerT2RzEgQT7Lt5FK"
	// Circle devnet USDC (test_mode ⇒ devnet). NOT the mainnet EPjFW... mint.
	devnetUSDCMint = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
	// 1 USDC = 10^6 base units. The real on-chain leg moves exactly this.
	oneUSDC = uint64(1_000_000)
)

func TestSolanaDevnetMoneyMovementProof(t *testing.T) {
	base58Key := strings.TrimSpace(os.Getenv(solanaPrivateKeyEnv))
	if base58Key == "" {
		t.Skipf("%s not set; skipping Solana money-movement proof (opt-in)", solanaPrivateKeyEnv)
	}

	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	// --- Merchant Solana key lives as a PSP SECRET, not on-chain. ----
	// The private key is scoped to the Solana PSP and resolved
	// through the production PSP signer — proving the money-signing
	// authority is internal OpenRails state, never carried in any Solana payload.
	priv := solanago.MustPrivateKeyFromBase58(base58Key)
	merchantPub := priv.PublicKey()
	require.Equal(t, expectedSolanaWallet, merchantPub.String(),
		"configured private key must derive the documented merchant wallet (fail loud on a swapped key)")

	secretStore, err := merchants.NewDBSecretStore(db.WrapPool(h.Pool(), ""))
	require.NoError(t, err)
	environment := "test"
	now := time.Now().UTC()
	require.NoError(t, surface.app.Runtime.DB.RunInMerchantConn(merchant.WithID(ctx, dbtest.TestMerchantID), func(ctx context.Context) error {
		_, err := surface.app.Runtime.DB.Gen(ctx).UpsertPSP(ctx, gen.UpsertPSPParams{
			MerchantID:     dbtest.TestMerchantID.UUID(),
			Rail:           "solana",
			Environment:    &environment,
			AccountID:      merchantPub.String(),
			Evidence:       []byte(`{"signer":{"mode":"local_keypair"}}`),
			LastVerifiedAt: &now,
		})
		return err
	}))
	secretName, err := merchants.PSPSecretName("solana", environment, merchantPub.String(), "private_key")
	require.NoError(t, err)
	_, err = secretStore.Put(ctx, dbtest.TestMerchantID, secretName, base58Key)
	require.NoError(t, err, "inject PSP private_key secret")

	signer := recurring.NewSignerFromPSPs(secretStore, nil, surface.app.Runtime.DB, 0, environment)
	signerPub, err := signer.PublicKey(ctx, dbtest.TestMerchantID)
	require.NoError(t, err, "production signer must resolve the PSP key from the secret store")
	require.Equal(t, merchantPub, signerPub, "secret-store-backed signer derives the merchant wallet")

	// --- (b) DB is the source of truth: publish a Solana-priced catalog product.
	const (
		displayName = "Solana Pro Plan"
		description = "premium tier billed over the Solana payment rail"
		entitlement = "solana-pro-premium"
		creditKey   = "solana-pro-usd"
		priceMicros = int64(19_990_000)
	)
	productKey := "solana-pro-" + strings.ReplaceAll(uuid.NewString(), "-", "")

	catalogToken := surface.MintAPIKey(
		dbtest.TestMerchantSlug,
		"solana-catalog-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate},
	)

	manifest := catalog.Manifest{
		Version:        catalog.SupportedVersion,
		CreditBalances: []catalog.CreditBalance{{Key: creditKey, Unit: "USD"}},
		Products: []catalog.Product{{
			Key:          productKey,
			DisplayName:  displayName,
			Description:  description,
			Entitlements: []string{entitlement},
			Credits:      []catalog.CreditGrant{{Key: creditKey, Currency: "USD", Amount: solanaPtrI64(50_000)}},
			Prices: []catalog.Price{{
				UnitAmount: priceMicros,
				Currency:   "USD",
				Duration:   "30d",
				AutoRenew:  true,
				PSPs:       []string{"solana"}, // priced for the Solana rail
			}},
		}},
	}
	require.NoError(t, manifest.Validate())

	publishStatus, publishBody := requestJSON(t, "POST", surface.BaseURL+"/v1/merchant/catalog/publish", catalogToken, map[string]any{
		"catalog": manifest,
		"insert":  true,
	})
	require.Equal(t, 200, publishStatus, string(publishBody))

	// The catalog/product/price/benefit metadata is persisted in Postgres (the
	// source of truth) — read it straight from the products table.
	var (
		dbProductKey, dbDisplayName, dbDescription string
		entitlementsSpec, creditsSpec              []byte
	)
	err = h.Pool().QueryRow(ctx, `
		SELECT key, display_name, description, entitlements_spec, credits_spec
          FROM openrails.products
		 WHERE merchant_id = $1::uuid AND key = $2
	`, dbtest.TestMerchantID.String(), productKey).Scan(&dbProductKey, &dbDisplayName, &dbDescription, &entitlementsSpec, &creditsSpec)
	require.NoError(t, err, "product metadata must be persisted in openrails.products")
	require.Equal(t, productKey, dbProductKey)
	require.Equal(t, displayName, dbDisplayName)
	require.Equal(t, description, dbDescription)
	require.Contains(t, string(entitlementsSpec), entitlement, "granted benefit (entitlement) stored in DB")
	require.Contains(t, string(creditsSpec), creditKey, "granted benefit (credits) stored in DB")

	// Resolve the published product + price ids (used by the DB payment row below).
	getStatus, getBody := requestJSON(t, "GET", surface.BaseURL+"/v1/merchant/catalog/products/by-key/"+productKey, catalogToken, nil)
	require.Equal(t, 200, getStatus, string(getBody))
	var product billingservice.CatalogProduct
	require.NoError(t, json.Unmarshal(getBody, &product))
	require.NotEqual(t, uuid.Nil, product.ID)

	prices, err := (httpCatalogApplier{t: t, baseURL: surface.BaseURL, token: catalogToken}).ListPricesByProduct(ctx, product.ID, true)
	require.NoError(t, err)
	require.Len(t, prices, 1)
	priceID := prices[0].ID

	// --- (a) The on-chain payload carries ONLY money-movement identifiers. ------
	// Build the Solana transfer with the PRODUCTION builder. The on-chain inputs
	// are: amount, token mint, recipient (ATA), payer, and a reference id. None of
	// the catalog/product/entitlement/credit metadata above is an input.
	reference, err := solanaint.GenerateReference()
	require.NoError(t, err, "generate Solana Pay reference id")

	tokenUnits, err := solanamod.FiatMicrosToStablecoinBaseUnits(ctx, moneyutil.Micros(priceMicros), "USDC", 6, nil)
	require.NoError(t, err)
	require.Equal(t, uint64(19_990_000), tokenUnits, "$19.99 in micro-USD at the $1 USDC peg = 19.99 USDC base units")

	rpcClient := solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{Network: "devnet"})

	// A real checkout payer is the customer; here we self-build with the merchant
	// wallet as payer purely to inspect the constructed payload. The recipient is
	// the merchant wallet (money moves TO the merchant).
	built, err := rpcClient.BuildTransferTransaction(ctx, solanaint.TransferRequest{
		FromWallet:  merchantPub.String(),
		ToWallet:    merchantPub.String(),
		TokenSymbol: "USDC",
		TokenMint:   devnetUSDCMint,
		Amount:      tokenUnits,
		Reference:   reference,
	})
	require.NoError(t, err, "BuildTransferTransaction must succeed when the key is configured (fail loud)")
	require.NotEmpty(t, built.TransactionBase64)

	builtTx := decodeTx(t, built.TransactionBase64)

	// Positive: the payload carries the money-movement identifiers.
	require.Equal(t, merchantPub, builtTx.Message.AccountKeys[0], "fee payer is the merchant wallet")
	require.True(t, txContainsKey(builtTx, solanago.MustPublicKeyFromBase58(reference)),
		"payment reference id is carried on-chain (the only link back to OpenRails state)")
	gotAmount, ok := tokenTransferAmount(t, builtTx)
	require.True(t, ok, "a token transfer instruction is present")
	require.Equal(t, tokenUnits, gotAmount, "on-chain amount equals the money-movement amount")

	// Negative (the crux of proof (a)): NONE of the catalog/product/entitlement
	// metadata appears anywhere in the serialized on-chain transaction.
	assertNoMetadataInTx(t, built.TransactionBase64, productKey, displayName, description, entitlement, creditKey)

	// --- (b) Granted benefits + invoice/payment state live in the OpenRails DB. -
	proveDBSourceOfTruth(t, h, surface, product.ID, priceID, reference, entitlement)

	// --- Optional real on-chain leg: a funded devnet USDC transfer. -------------
	maybeRunOnChainLeg(t, ctx, rpcClient, priv, merchantPub,
		productKey, displayName, description, entitlement, creditKey)
}

// proveDBSourceOfTruth grants the catalog entitlement, finalizes an invoice, and
// records a Solana payment row — all internal OpenRails state — then asserts the
// payment's benefit snapshot is held in Postgres keyed to the on-chain reference
// id (and is NOT something Solana ever saw).
func proveDBSourceOfTruth(t *testing.T, h *Harness, surface *Surface, productID, priceID uuid.UUID, reference, entitlement string) {
	t.Helper()
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	payer := openrails.CustomerID(uuid.New())
	payerID := payer.UUID()
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, payerID.String())
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payerID)
	})

	// Granted benefit: the entitlement OpenRails grants from the catalog product.
	ledger := grants.New(gen.New(pool), dbtest.TestMerchantID.UUID())
	g, err := ledger.Grant(ctx, grants.GrantInput{
		Customer: payerID,
		Product:  &productID,
		Kind:     grants.Entitlement,
		Source:   grants.Purchase,
		SourceID: reference, // tie the grant to the Solana reference id
		Spec:     &grants.Spec{Entitlements: []string{entitlement}},
	})
	require.NoError(t, err)
	require.NoError(t, ledger.MaterializeGrant(ctx, g))

	entStatus, entBody := requestJSON(t, "GET", surface.BaseURL+"/v1/merchant/customers/"+payerID.String()+"/entitlements", surface.Token, nil)
	require.Equal(t, 200, entStatus, string(entBody))
	require.Contains(t, string(entBody), entitlement, "granted entitlement is served from OpenRails DB")

	// Invoice state: a usage capture that finalizes an invoice in Postgres.
	client := surface.Client()
	depositSourceID := uuid.NewString()
	_, err = client.DepositCredits(ctx, openrails.DepositCreditsRequest{
		CustomerID: &payer,
		Invoker:    payerID.String(),
		Currency:   "USD",
		Amount:     50_000,
		Source:     "solana-money-movement",
		SourceID:   depositSourceID,
	})
	require.NoError(t, err)

	requestID := "solana-proof-" + uuid.NewString()
	verdicts, err := client.AdmitBatch(ctx, []openrails.AdmitRequest{{
		CustomerID:      payerID.String(),
		Invoker:         payerID.String(),
		InvokerType:     string(identity.InvokerTypePayer),
		Resource:        "vm-small",
		Currency:        "USD",
		EstimatedAmount: 2_500,
		ExpiresAt:       holdDeadline(),
		RequestID:       requestID,
		Source:          "solana-money-movement",
	}})
	require.NoError(t, err)
	require.Len(t, verdicts, 1)
	require.True(t, verdicts[0].Allowed(), "%+v", verdicts[0])
	require.NoError(t, client.Capture(ctx, requestID, 2_000, &openrails.CaptureUsage{
		EventType: "vm-runtime",
		Resource:  "vm-small",
		Source:    "solana-money-movement",
		SourceID:  requestID,
	}))

	from := time.Now().Add(-time.Hour)
	to := time.Now().Add(time.Hour)
	inv, err := money.NewMoneyService(dbi).FinalizeInvoice(ctx, identity.CustomerID(payerID), money.DefaultCurrency, from, to)
	require.NoError(t, err)
	require.Equal(t, int64(2_000), inv.UsageTotal, "invoice usage state lives in OpenRails DB")

	// Payment state: a Solana payment row whose only on-chain linkage is the
	// reference id, while the benefit snapshot (entitlements) is held in Postgres.
	solanaPSP := dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), "solana")
	var paymentID uuid.UUID
	var snapshot []byte
	err = pool.QueryRow(ctx, `
		INSERT INTO openrails.payments
			(merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, entitlements_spec_snapshot, psp_id)
		VALUES ($1::uuid, $2, $3, 'solana', $4, $5, $5, 'USD', 'completed', $6::jsonb, $7)
		RETURNING id, entitlements_spec_snapshot
	`, dbtest.TestMerchantID.String(), payerID, priceID, reference, int64(19_990_000),
		`{"entitlements":["`+entitlement+`"]}`, solanaPSP).Scan(&paymentID, &snapshot)
	require.NoError(t, err, "Solana payment + benefit snapshot recorded in openrails.payments")
	require.NotEqual(t, uuid.Nil, paymentID)
	require.Contains(t, string(snapshot), entitlement,
		"benefit snapshot for the Solana payment is held in OpenRails DB, keyed to the on-chain reference id")
}

// maybeRunOnChainLeg submits a real devnet USDC transfer when the wallet is
// funded. Submission/confirmation failures are LOGGED, not fatal (devnet is a
// flaky external dependency and the metadata proof is the acceptance).
func maybeRunOnChainLeg(t *testing.T, ctx context.Context, rpcClient *solanaint.RPCClient, priv solanago.PrivateKey, merchantPub solanago.PublicKey, metadataNeedles ...string) {
	t.Helper()
	usdcMint := solanago.MustPublicKeyFromBase58(devnetUSDCMint)

	solBal, err := rpcClient.GetBalance(ctx, merchantPub)
	if err != nil {
		t.Logf("on-chain leg skipped: cannot read SOL balance (devnet RPC): %v", err)
		return
	}
	usdcBal, err := rpcClient.GetTokenBalanceForMint(ctx, merchantPub, usdcMint)
	if err != nil {
		t.Logf("on-chain leg skipped: cannot read USDC balance (devnet RPC): %v", err)
		return
	}
	t.Logf("devnet wallet %s balances: SOL=%d lamports, USDC=%d base units", merchantPub, solBal, usdcBal)

	if usdcBal < oneUSDC || solBal < 5_000_000 {
		t.Logf("on-chain leg skipped: wallet not sufficiently funded (need >= 1 USDC and ~0.005 SOL for fees/rent); operator may airdrop")
		return
	}

	// Fresh recipient; only an ATA is needed (it never signs).
	recipientKey, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	recipientPub := recipientKey.PublicKey()
	reference, err := solanaint.GenerateReference()
	require.NoError(t, err)

	// Tx A: create the recipient's USDC ATA (merchant pays rent), idempotent.
	createIx := ata.NewCreateIdempotentInstruction(merchantPub, recipientPub, usdcMint).Build()
	if !submitSigned(t, ctx, rpcClient, priv, merchantPub, []solanago.Instruction{createIx}, "create recipient ATA") {
		return
	}

	// Tx B: the production-built 1 USDC transfer, signed by the merchant.
	built, err := rpcClient.BuildTransferTransaction(ctx, solanaint.TransferRequest{
		FromWallet:  merchantPub.String(),
		ToWallet:    recipientPub.String(),
		TokenSymbol: "USDC",
		TokenMint:   devnetUSDCMint,
		Amount:      oneUSDC,
		Reference:   reference,
	})
	require.NoError(t, err, "build on-chain USDC transfer (fail loud)")
	// The real on-chain payload carries only money-movement identifiers.
	assertNoMetadataInTx(t, built.TransactionBase64, metadataNeedles...)

	tx := decodeTx(t, built.TransactionBase64)
	msg, err := tx.Message.MarshalBinary()
	require.NoError(t, err)
	sig, err := priv.Sign(msg)
	require.NoError(t, err)
	tx.Signatures = []solanago.Signature{sig}

	outcome, err := rpcClient.SubmitAndConfirm(ctx, tx, solanaint.ChainTerminal{})
	if err != nil {
		t.Logf("on-chain leg: submit/confirm failed (devnet flaky, non-fatal): %v", err)
		return
	}
	if oerr := outcome.OnChainError(); oerr != nil {
		t.Logf("on-chain leg: transfer reverted on-chain (non-fatal): %v", oerr)
		return
	}
	t.Logf("on-chain leg CONFIRMED: 1 USDC merchant->%s, signature=%s (devnet)", recipientPub, outcome.Signature)

	// Production VerifyTransfer asserts on-chain amount/recipient/reference/payer.
	verifyErr := rpcClient.VerifyTransfer(ctx, solanaint.VerifyTransferRequest{
		Signature:         outcome.Signature.String(),
		ExpectedAmount:    oneUSDC,
		ExpectedRecipient: recipientPub.String(),
		ExpectedTokenMint: devnetUSDCMint,
		ExpectedPayer:     merchantPub.String(),
		ExpectedReference: reference,
	})
	if verifyErr != nil {
		t.Logf("on-chain leg: VerifyTransfer not yet satisfied (confirmation lag, non-fatal): %v", verifyErr)
		return
	}
	t.Logf("on-chain leg VERIFIED on devnet: amount=%d mint=%s reference=%s", oneUSDC, devnetUSDCMint, reference)
}

// submitSigned signs (merchant payer) and submits a one-off instruction set,
// returning false (logged) on any RPC failure so the caller can bail non-fatally.
func submitSigned(t *testing.T, ctx context.Context, rpcClient *solanaint.RPCClient, priv solanago.PrivateKey, payer solanago.PublicKey, instructions []solanago.Instruction, label string) bool {
	t.Helper()
	blockhash, err := rpcClient.GetLatestBlockhash(ctx)
	if err != nil {
		t.Logf("on-chain leg skipped at %q: blockhash: %v", label, err)
		return false
	}
	tx, err := solanago.NewTransaction(instructions, blockhash, solanago.TransactionPayer(payer))
	require.NoError(t, err, label)
	msg, err := tx.Message.MarshalBinary()
	require.NoError(t, err, label)
	sig, err := priv.Sign(msg)
	require.NoError(t, err, label)
	tx.Signatures = []solanago.Signature{sig}
	outcome, err := rpcClient.SubmitAndConfirm(ctx, tx, solanaint.ChainTerminal{})
	if err != nil {
		t.Logf("on-chain leg: %q submit/confirm failed (non-fatal): %v", label, err)
		return false
	}
	if oerr := outcome.OnChainError(); oerr != nil {
		t.Logf("on-chain leg: %q reverted (non-fatal): %v", label, oerr)
		return false
	}
	t.Logf("on-chain leg: %q confirmed, signature=%s", label, outcome.Signature)
	return true
}

// --- helpers ---------------------------------------------------------------

func decodeTx(t *testing.T, b64 string) *solanago.Transaction {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	require.NoError(t, err)
	tx, err := solanago.TransactionFromBytes(raw)
	require.NoError(t, err)
	return tx
}

func txContainsKey(tx *solanago.Transaction, key solanago.PublicKey) bool {
	for _, k := range tx.Message.AccountKeys {
		if k.Equals(key) {
			return true
		}
	}
	return false
}

func tokenTransferAmount(t *testing.T, tx *solanago.Transaction) (uint64, bool) {
	t.Helper()
	for _, inst := range tx.Message.Instructions {
		progID, err := tx.ResolveProgramIDIndex(inst.ProgramIDIndex)
		if err != nil || !progID.Equals(token.ProgramID) {
			continue
		}
		accs, err := inst.ResolveInstructionAccounts(&tx.Message)
		if err != nil {
			continue
		}
		decoded, err := token.DecodeInstruction(accs, inst.Data)
		if err != nil {
			continue
		}
		if tr, ok := decoded.Impl.(*token.Transfer); ok && tr.Amount != nil {
			return *tr.Amount, true
		}
	}
	return 0, false
}

func solanaPtrI64(v int64) *int64 { return &v }

// assertNoMetadataInTx is the heart of proof (a): no catalog/product/entitlement
// metadata may appear anywhere in the serialized on-chain transaction bytes.
func assertNoMetadataInTx(t *testing.T, txBase64 string, needles ...string) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(txBase64)
	require.NoError(t, err)
	hay := string(raw)
	for _, n := range needles {
		if strings.TrimSpace(n) == "" {
			continue
		}
		require.NotContains(t, hay, n,
			"on-chain payload must NOT carry catalog/product metadata %q (Solana is a payment rail only)", n)
	}
}
