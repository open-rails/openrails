package money

import (
	"testing"
	"time"
)

func TestRateMeteredAggregateCounterRoundsOnce(t *testing.T) {
	got, err := RateMeteredAggregate(3, MeteredRate{
		Kind:       MeterKindCounter,
		RateMicros: 2,
		PerUnits:   10,
	})
	if err != nil {
		t.Fatalf("RateMeteredAggregate: %v", err)
	}
	if got != 1 {
		t.Fatalf("aggregate rounding = %d, want 1", got)
	}
}

func TestRateMeteredAggregateCounterPerUnits(t *testing.T) {
	got, err := RateMeteredAggregate(1_000_000, MeteredRate{
		Kind:       MeterKindCounter,
		RateMicros: 200_000,
		PerUnits:   1_000_000,
	})
	if err != nil {
		t.Fatalf("RateMeteredAggregate: %v", err)
	}
	if got != 200_000 {
		t.Fatalf("counter amount = %d, want 200000", got)
	}
}

func TestRateMeteredAggregateGaugeTimeIntegrated(t *testing.T) {
	got, err := RateMeteredAggregate(2*3600, MeteredRate{
		Kind:       MeterKindGauge,
		RateMicros: 50_000,
		PerUnits:   1,
		Per:        time.Hour,
	})
	if err != nil {
		t.Fatalf("RateMeteredAggregate: %v", err)
	}
	if got != 100_000 {
		t.Fatalf("gauge amount = %d, want 100000", got)
	}
}

func TestRateMeteredAggregateRejectsInvalidGauge(t *testing.T) {
	_, err := RateMeteredAggregate(1, MeteredRate{Kind: MeterKindGauge, RateMicros: 1})
	if err == nil {
		t.Fatal("expected gauge without per duration to fail")
	}
}
