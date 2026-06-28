package handlers

import "testing"

func TestCatalogPublishPlanOnly(t *testing.T) {
	if !catalogPublishPlanOnly(merchantCatalogPublishRequest{}) {
		t.Fatal("empty request should be plan-only")
	}
	if catalogPublishPlanOnly(merchantCatalogPublishRequest{Insert: true}) {
		t.Fatal("insert should apply")
	}
	yes := true
	if !catalogPublishPlanOnly(merchantCatalogPublishRequest{Insert: true, PlanOnly: &yes}) {
		t.Fatal("explicit plan_only should win")
	}
}
