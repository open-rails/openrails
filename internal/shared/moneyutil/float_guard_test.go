package moneyutil

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestNoFloatsInMoneyPackages enforces MONEY-3: currency and crypto amounts are
// INTEGERS. No float may represent, convert, round, or compare an amount.
//
// A *rate* may legitimately be a float (an FX quote, a token price feed) —
// arithmetic that produces or compares an *amount* may not. Every float that
// survives in a guarded package is therefore a rate, a rate→exact-rational
// converter, or a named residual, and each carries its justification below.
//
// Shape is deliberately the same as config.TestNoLibraryEnvReads: a walk, a
// needle set, an allowlist with a one-line justification per entry, a failing
// test. It differs in one way that matters — it parses the AST rather than
// grepping lines, so a float that arrives through a wrapper, a struct field, a
// method value, or a renamed import is still visible, and the allowlist is
// keyed by DECLARATION rather than by file. Adding a second float to an
// already-allowlisted file fails.
func TestNoFloatsInMoneyPackages(t *testing.T) {
	// Packages where amounts live. Every one of these is float-free today
	// except for the allowlisted rate sites below — keep it that way.
	guarded := []string{
		"internal/shared/moneyutil/",
		"internal/modules/money/",
		"internal/modules/payments/",
		"internal/modules/paymentmethods/",
		"internal/modules/grants/",
		"internal/modules/budgets/",
		"internal/modules/entitlements/",
		"internal/modules/checkout/",
		"internal/modules/subscriptions/",
		"internal/modules/admission/",
		"internal/modules/solana/",
		"internal/modules/webhooks/",
		"internal/modules/reconcile/",
		// or#863: the guard listed internal/modules/reconcile/ and stopped —
		// but the reconcile ENGINE is internal/reconcile/, a different package,
		// and it held a live violation (a float64 on-chain token amount). The
		// packages below were the guard's holes; each one reaches an amount.
		"internal/reconcile/",
		"internal/http/handlers/",
		"internal/river/",
		"internal/integrations/pyth/",
		"internal/integrations/basistheory/",
		"internal/integrations/solana/",
		"internal/modules/payments/",
		"internal/controlplane/",
		"internal/intents/",
		"internal/integrations/fx/",
		"internal/integrations/nmi/",
		"internal/integrations/ccbill/",
		"internal/integrations/stripeapi/",
		"pkg/pricing/",
		"pkg/catalog/",
		"pkg/service/",
		"pkg/api/",
	}

	// "<path>:<top-level declaration>" -> why this float is not an amount.
	// A float anywhere else in a guarded package FAILS.
	allowed := map[string]string{
		// --- FX: the rate is a float; the amount is a big.Rat -------------
		"internal/integrations/fx/provider.go:Quote":         "Quote.Rate is the RATE itself — the one value MONEY-3 permits as a float",
		"internal/integrations/fx/provider.go:ratFromRate":   "rate -> exact rational converter; the boundary where float stops",
		"internal/integrations/fx/mock.go:MockProvider":      "test-double RATE table",
		"internal/integrations/fx/mock.go:NewMockProvider":   "test-double RATE table",
		"internal/integrations/fx/mock.go:SetRate":           "test-double RATE setter",
		"internal/integrations/fx/exchange_api.go:fetchRate": "parses the provider's quoted RATE",
		"internal/integrations/fx/exchange_api.go:Quote":     "identity RATE 1.0 for same-currency quotes",
		"internal/integrations/fx/redis_cache.go:redisRate":  "cached RATE payload",

		// --- Solana: token price + FX rate are rates; amounts are big.Int --
		"internal/modules/solana/types.go:PayResult":                         "TokenPriceUSD/FXRate are RATES; the token amount is uint64 base units",
		"internal/modules/solana/support.go:TokenQuote":                      "TokenPriceUSD/FXRate are RATES; Units (the amount) is uint64",
		"internal/modules/solana/support.go:TokenPriceProvider":              "price-feed interface returns a RATE",
		"internal/modules/solana/support.go:stablecoinPriceUSD":              "peg-divergence check on a RATE (math.Abs over a price, never an amount)",
		"internal/modules/solana/support.go:stablecoinPegTolerance":          "peg tolerance is a fraction of a RATE, not an amount",
		"internal/modules/solana/support.go:ratFromRate":                     "rate -> exact rational converter; the boundary where float stops",
		"internal/modules/solana/support.go:fiatMicrosToBaseUnitsAtRate":     "takes RATES as floats, converts both to big.Rat before any amount arithmetic",
		"internal/modules/solana/support.go:microsAtRate":                    "takes a RATE as a float, converts to big.Rat before any amount arithmetic",
		"internal/modules/solana/support.go:CalculateTokenQuote":             "carries the token price + FX RATE; both amount branches go through big.Rat/big.Int",
		"internal/modules/solana/support.go:FiatMicrosToStablecoinBaseUnits": "passes the identity FX RATE 1.0 into the exact-rational converter",

		// --- Checkout: the persisted quote's rates ------------------------
		"internal/modules/checkout/session_service.go:setSolanaQuoteState": "persists token_price_usd / fx_rate (RATES); token_amount is written as a decimal string",

		// --- Solana token feed + display: rates, not amounts ---------------
		"internal/integrations/pyth/client.go:Price":                           "Pyth Price/Conf are the feed's quoted RATE and its confidence interval",
		"internal/integrations/pyth/client.go:PriceUSD":                        "returns the token's USD RATE; no amount is computed here",
		"internal/integrations/pyth/client.go:validateParsedPrice":             "sanity-bounds the parsed RATE",
		"internal/integrations/pyth/client.go:parsePythNumber":                 "parses the feed's mantissa+exponent RATE encoding; the boundary where float starts, never an amount",
		"internal/http/handlers/solana_supported_tokens.go:TokenInfo":          "TokenInfo.Price is the token's USD RATE for display",
		"internal/http/handlers/solana_supported_tokens.go:TokenQuote":         "TokenPriceUSD/FXRate are RATES; the quoted units are a decimal string",
		"internal/http/handlers/solana_supported_tokens.go:GetSupportedTokens": "builds a symbol -> RATE map for the token list response",

		// --- reconcile: fractions, counts and durations --------------------
		"internal/reconcile/cancel_budget.go:DefaultMaxCancelsPerPass":    "0.05 / 0.10 are cancel-budget FRACTIONS of a roster, not amounts",
		"internal/reconcile/cancel_budget.go:CancelBudget":                "Fraction is a share of the local live roster",
		"internal/reconcile/cancel_budget.go:fraction":                    "returns that share",
		"internal/reconcile/cancel_budget.go:Limit":                       "math.Floor over a COUNT of subscriptions the pass may cancel",
		"internal/reconcile/cancel_budget.go:RosterBreaker":               "Ratio is a remote/local roster-size ratio",
		"internal/reconcile/cancel_budget.go:ratio":                       "returns that ratio",
		"internal/reconcile/cancel_budget.go:Implausible":                 "compares two roster COUNTS against the ratio",
		"internal/reconcile/engine.go:Engine":                             "CircuitBreakerRatio is the roster-size ratio above",
		"internal/reconcile/store.go:EpisodeSummary":                      "TotalDays is an error-day DURATION total, not an amount",
		"internal/reconcile/mutations.go:mutationRecordsForFinding":       "decodes rows_affected from evidence — a ROW COUNT",
		"internal/river/jobs_dunning.go:manualRebillEvidenceResponseCode": "decodes the gateway RESPONSE CODE from intent evidence — not an amount",

		// --- Not amounts at all -------------------------------------------
		"internal/http/handlers/admin_findings_actions.go:paramAmountMicros":   "names float64/float32 only to REJECT them: this is the or#863 fix, the guard seeing its own refusal",
		"internal/http/handlers/merchant_metrics.go:MerchantMetricsAsk":        "math.Ceil over a Retry-After DURATION in seconds, not an amount",
		"internal/http/handlers/merchant_catalog_copilot.go:CatalogCopilotAsk": "math.Ceil over a Retry-After DURATION in seconds, not an amount",
		"internal/modules/admission/spendgate/gate.go:toInt64":                 "decodes a Redis Lua reply — an allow flag and a window INDEX, never an amount",
		"internal/modules/admission/admitter.go:Admit":                         "math.Ceil over a retry-after DURATION in seconds (or#822), not over an amount",
		"pkg/service/types.go:SolanaToken":                                     "SolanaToken.Price is the token's USD RATE for display, not an amount",
	}

	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected module root at %s: %v", root, err)
	}

	var violations []string
	unused := map[string]bool{}
	for k := range allowed {
		unused[k] = true
	}

	for _, prefix := range guarded {
		dir := filepath.Join(root, filepath.FromSlash(prefix))
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("guarded package %s does not exist — fix the list", prefix)
		}
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); strings.HasPrefix(name, ".") || name == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			rel = filepath.ToSlash(rel)
			if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
				return nil // test files may model wire floats freely
			}
			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return fmt.Errorf("parse %s: %w", rel, perr)
			}
			for _, hit := range floatHits(fset, file) {
				key := rel + ":" + hit.decl
				if _, ok := allowed[key]; ok {
					delete(unused, key)
					continue
				}
				violations = append(violations, fmt.Sprintf(
					"%s:%d: %s (in %s) — MONEY-3: an amount may not touch a float. "+
						"If this is a RATE, add %q to the allowlist with a one-line justification.",
					rel, hit.line, hit.what, hit.decl, key))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	sort.Strings(violations)
	if len(violations) > 0 {
		t.Errorf("float in a money package (MONEY-3 — currency and crypto amounts are integers only):\n  %s",
			strings.Join(violations, "\n  "))
	}

	stale := make([]string, 0, len(unused))
	for k := range unused {
		stale = append(stale, k)
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		// A stale entry means the float is gone, or the declaration was
		// renamed and the guard silently stopped covering it.
		t.Errorf("allowlist entries no longer match any float — delete them (or the declaration moved and is now unguarded):\n  %s",
			strings.Join(stale, "\n  "))
	}
}

type floatHit struct {
	decl string
	line int
	what string
}

// floatHits reports every float type reference and every float-shaped stdlib
// call in a file, attributed to the top-level declaration containing it.
func floatHits(fset *token.FileSet, file *ast.File) []floatHit {
	var hits []floatHit
	for _, d := range file.Decls {
		for _, name := range declNames(d) {
			ast.Inspect(d, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.Ident:
					if x.Name == "float32" || x.Name == "float64" {
						hits = append(hits, floatHit{decl: name, line: fset.Position(x.Pos()).Line, what: x.Name})
					}
				case *ast.BasicLit:
					// An untyped float literal is how a float sneaks in
					// without ever naming float64 — `amount * 1.5`.
					if x.Kind == token.FLOAT {
						hits = append(hits, floatHit{decl: name, line: fset.Position(x.Pos()).Line, what: "float literal " + x.Value})
					}
				case *ast.SelectorExpr:
					pkg, ok := x.X.(*ast.Ident)
					if !ok {
						return true
					}
					switch pkg.Name + "." + x.Sel.Name {
					case "strconv.ParseFloat", "strconv.FormatFloat",
						"math.Ceil", "math.Floor", "math.Round", "math.Pow10", "math.Pow",
						"math.Abs", "math.Trunc", "math.Inf", "math.NaN":
						hits = append(hits, floatHit{decl: name, line: fset.Position(x.Pos()).Line, what: pkg.Name + "." + x.Sel.Name})
					}
				}
				return true
			})
			break // attribute the whole decl to its first name
		}
	}
	return hits
}

// declNames returns the top-level names a declaration introduces. A GenDecl
// with several specs yields the first — enough to key an allowlist entry.
func declNames(d ast.Decl) []string {
	switch x := d.(type) {
	case *ast.FuncDecl:
		return []string{x.Name.Name}
	case *ast.GenDecl:
		var names []string
		for _, spec := range x.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				names = append(names, s.Name.Name)
			case *ast.ValueSpec:
				for _, n := range s.Names {
					names = append(names, n.Name)
				}
			}
		}
		return names
	}
	return nil
}
