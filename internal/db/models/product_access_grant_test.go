package models

import (
	"testing"
	"time"
)

func TestProductAccessGrant_IsActiveAt(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	hourAgo := now.Add(-time.Hour)
	hourLater := now.Add(time.Hour)

	cases := []struct {
		name  string
		grant ProductAccessGrant
		at    time.Time
		want  bool
	}{
		{
			name:  "active indefinite, started => active",
			grant: ProductAccessGrant{Status: ProductAccessStatusActive, StartsAt: hourAgo},
			at:    now,
			want:  true,
		},
		{
			name:  "active within window => active",
			grant: ProductAccessGrant{Status: ProductAccessStatusActive, StartsAt: hourAgo, EndsAt: &hourLater},
			at:    now,
			want:  true,
		},
		{
			name:  "not yet started => inactive",
			grant: ProductAccessGrant{Status: ProductAccessStatusActive, StartsAt: hourLater},
			at:    now,
			want:  false,
		},
		{
			name:  "ended => inactive",
			grant: ProductAccessGrant{Status: ProductAccessStatusActive, StartsAt: hourAgo, EndsAt: &hourAgo},
			at:    now,
			want:  false,
		},
		{
			name:  "revoked status => inactive",
			grant: ProductAccessGrant{Status: ProductAccessStatusRevoked, StartsAt: hourAgo},
			at:    now,
			want:  false,
		},
		{
			name:  "revoked_at set => inactive",
			grant: ProductAccessGrant{Status: ProductAccessStatusActive, StartsAt: hourAgo, RevokedAt: &now},
			at:    now,
			want:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.grant.IsActiveAt(tc.at); got != tc.want {
				t.Fatalf("IsActiveAt() = %v, want %v", got, tc.want)
			}
		})
	}
}
