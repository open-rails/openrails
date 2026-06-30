package pricing

import "testing"

func i64(v int64) *int64 { return &v }

// graduatedTiers: $0.0100/credit to 2k, $0.0090 to 10k, $0.0080 beyond.
func graduatedTiers() []ChargeTier {
	return []ChargeTier{
		{UpTo: i64(2_000), UnitAmount: 10_000},
		{UpTo: i64(10_000), UnitAmount: 9_000},
		{UpTo: nil, UnitAmount: 8_000},
	}
}

func TestRate_PerUnitProRatesAndRoundsOnce(t *testing.T) {
	// DO droplet: $0.00595/hr (5_950 micros) billed per-second. 5400s = 1.5h.
	cm := ChargeModel{Kind: ModelPerUnit, UnitAmount: 5_950, DivideBy: 3_600}
	got, err := cm.Rate(5_400)
	if err != nil {
		t.Fatal(err)
	}
	if got != 8_925 { // 5400*5950/3600 = 8925 exactly
		t.Fatalf("per_unit pro-rate = %d, want 8925", got)
	}
}

func TestRate_PerUnitRoundingModes(t *testing.T) {
	// 2 * 10 / 3 = 6.667
	cases := []struct {
		round string
		want  int64
	}{
		{RoundHalfUp, 7},
		{RoundUp, 7},
		{RoundDown, 6},
	}
	for _, c := range cases {
		cm := ChargeModel{Kind: ModelPerUnit, UnitAmount: 10, DivideBy: 3, Round: c.round}
		got, err := cm.Rate(2)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Fatalf("round %q = %d, want %d", c.round, got, c.want)
		}
	}
	// 1 * 10 / 3 = 3.333 -> up=4, half_up=3, down=3
	for _, c := range []struct {
		round string
		want  int64
	}{{RoundUp, 4}, {RoundHalfUp, 3}, {RoundDown, 3}} {
		cm := ChargeModel{Kind: ModelPerUnit, UnitAmount: 10, DivideBy: 3, Round: c.round}
		got, _ := cm.Rate(1)
		if got != c.want {
			t.Fatalf("round %q of 3.333 = %d, want %d", c.round, got, c.want)
		}
	}
}

func TestRate_Cap(t *testing.T) {
	// Monthly cap: a droplet running far past 672h still bills only the cap. There
	// is no per-line floor — sub-cent lines are intentional (#642); minimums live
	// at the account level (minimum_spend, #643).
	cap := ChargeModel{Kind: ModelPerUnit, UnitAmount: 5_950, DivideBy: 3_600, MaximumAmount: 4_000_000}
	got, _ := cap.Rate(3_000 * 3_600) // 3000 hours
	if got != 4_000_000 {
		t.Fatalf("monthly cap = %d, want 4000000", got)
	}
	// Below a cent, a tiny window bills its true pro-rated cost (no floor).
	small := ChargeModel{Kind: ModelPerUnit, UnitAmount: 5_950, DivideBy: 3_600, Round: RoundUp}
	if got, _ := small.Rate(30); got != 50 { // 30s * 5950/3600 = 49.58 -> 50
		t.Fatalf("sub-cent line = %d, want 50 (no floor)", got)
	}
}

func TestRate_TieredVolumeVsGraduated(t *testing.T) {
	// 5000 credits. volume: whole qty at the landed band (tier2 = 9_000).
	vol := ChargeModel{Kind: ModelTiered, Mode: TierModeVolume, Tiers: graduatedTiers()}
	if got, _ := vol.Rate(5_000); got != 45_000_000 { // 5000*9000
		t.Fatalf("volume 5000 = %d, want 45000000", got)
	}
	// graduated: 2000*10000 + 3000*9000 = 47,000,000.
	grad := ChargeModel{Kind: ModelTiered, Mode: TierModeGraduated, Tiers: graduatedTiers()}
	if got, _ := grad.Rate(5_000); got != 47_000_000 {
		t.Fatalf("graduated 5000 = %d, want 47000000", got)
	}
	// graduated 5333: 20,000,000 + 3333*9000 = 49,997,000.
	if got, _ := grad.Rate(5_333); got != 49_997_000 {
		t.Fatalf("graduated 5333 = %d, want 49997000", got)
	}
}

func TestRate_Package(t *testing.T) {
	// $5 / 100-credit block, 100 free; 201 credits -> free 100, ceil(101/100)=2 blocks.
	cm := ChargeModel{Kind: ModelPackage, PackageSize: 100, PackageAmount: 5_000_000, FreeUnits: 100}
	if got, _ := cm.Rate(201); got != 10_000_000 {
		t.Fatalf("package 201 = %d, want 10000000", got)
	}
	if got, _ := cm.Rate(100); got != 0 { // all free
		t.Fatalf("package 100 = %d, want 0", got)
	}
}

func TestRate_FlatAndDynamic(t *testing.T) {
	flat := ChargeModel{Kind: ModelFlat, FlatAmount: 5_000_000}
	if got, _ := flat.Rate(99); got != 5_000_000 {
		t.Fatalf("flat = %d, want 5000000", got)
	}
	dyn := ChargeModel{Kind: ModelDynamic, Multiplier: 1_500_000} // +50% markup
	if got, _ := dyn.Rate(1_000_000); got != 1_500_000 {
		t.Fatalf("dynamic = %d, want 1500000", got)
	}
}

func TestRate_GraduatedFlatPerTier(t *testing.T) {
	// tier1: $0.0001/unit + $0.05 flat for the band; tier2: $0.00005/unit.
	cm := ChargeModel{Kind: ModelTiered, Mode: TierModeGraduated, Tiers: []ChargeTier{
		{UpTo: i64(1_000), UnitAmount: 100, FlatAmount: 50_000},
		{UpTo: nil, UnitAmount: 50},
	}}
	// 500 units: all in tier1 -> 500*100 + 50000 = 100000.
	if got, _ := cm.Rate(500); got != 100_000 {
		t.Fatalf("graduated+flat 500 = %d, want 100000", got)
	}
	// 1500 units: 1000*100+50000 (tier1) + 500*50 (tier2) = 150000 + 25000 = 175000.
	if got, _ := cm.Rate(1_500); got != 175_000 {
		t.Fatalf("graduated+flat 1500 = %d, want 175000", got)
	}
}

// Property: across spends, the quote never grants more value than paid, and is
// maximal (one more unit would exceed the budget). Guards the credit-purchase
// money path against off-by-one over-granting.
func TestQuoteUnitsForSpend_NeverOverGrantsAndIsMaximal(t *testing.T) {
	cm := ChargeModel{Kind: ModelTiered, Mode: TierModeGraduated, Tiers: graduatedTiers()}
	for _, spend := range []int64{0, 1, 9_999, 10_000, 19_999_999, 20_000_001, 49_999_999, 100_000_000} {
		units, cost, err := QuoteUnitsForSpend(spend, cm)
		if err != nil {
			t.Fatalf("spend %d: %v", spend, err)
		}
		if cost > spend {
			t.Fatalf("spend %d: quoted cost %d exceeds budget (over-granted)", spend, cost)
		}
		next, err := cm.Rate(units + 1)
		if err != nil {
			t.Fatal(err)
		}
		if next <= spend {
			t.Fatalf("spend %d: quote of %d units is not maximal (units+1 costs %d <= %d)", spend, units, next, spend)
		}
	}
}

// The credit-purchase "enter $ -> credits" direction, and its inverse, must agree.
func TestQuoteUnitsForSpend_GraduatedBidirectional(t *testing.T) {
	cm := ChargeModel{Kind: ModelTiered, Mode: TierModeGraduated, Tiers: graduatedTiers()}

	// $20 buys exactly 2000 credits at the base rate.
	credits, cost, err := QuoteUnitsForSpend(20_000_000, cm)
	if err != nil {
		t.Fatal(err)
	}
	if credits != 2_000 || cost != 20_000_000 {
		t.Fatalf("$20 -> (%d credits, %d cost), want (2000, 20000000)", credits, cost)
	}

	// $50 buys 5333 credits (floored); never grants more value than paid.
	credits, cost, err = QuoteUnitsForSpend(50_000_000, cm)
	if err != nil {
		t.Fatal(err)
	}
	if credits != 5_333 || cost != 49_997_000 {
		t.Fatalf("$50 -> (%d credits, %d cost), want (5333, 49997000)", credits, cost)
	}
	if cost > 50_000_000 {
		t.Fatalf("quoted cost %d exceeds spend", cost)
	}
	// Inverse agrees: pricing the quoted credits reproduces the quoted cost.
	if back, _ := cm.Rate(credits); back != cost {
		t.Fatalf("inverse mismatch: Rate(%d)=%d, want %d", credits, back, cost)
	}
}
