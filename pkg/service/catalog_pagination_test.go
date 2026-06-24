package service

import "testing"

func TestClampCatalogPage(t *testing.T) {
	tests := []struct {
		name         string
		limit        int
		offset       int
		wantLimit    int
		wantOffset   int
	}{
		{
			name:       "zero limit defaults to 100",
			limit:      0,
			offset:     0,
			wantLimit:  100,
			wantOffset: 0,
		},
		{
			name:       "negative limit defaults to 100",
			limit:      -1,
			offset:     0,
			wantLimit:  100,
			wantOffset: 0,
		},
		{
			name:       "positive limit within bounds passes through",
			limit:      50,
			offset:     0,
			wantLimit:  50,
			wantOffset: 0,
		},
		{
			name:       "limit at exact cap passes through",
			limit:      1000,
			offset:     0,
			wantLimit:  1000,
			wantOffset: 0,
		},
		{
			name:       "limit above cap is clamped to maxCatalogPageSize",
			limit:      10_000_000,
			offset:     0,
			wantLimit:  maxCatalogPageSize,
			wantOffset: 0,
		},
		{
			name:       "negative offset is floored to 0",
			limit:      50,
			offset:     -5,
			wantLimit:  50,
			wantOffset: 0,
		},
		{
			name:       "positive offset passes through",
			limit:      50,
			offset:     200,
			wantLimit:  50,
			wantOffset: 200,
		},
		{
			name:       "zero limit and negative offset both normalised",
			limit:      0,
			offset:     -99,
			wantLimit:  100,
			wantOffset: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotLimit, gotOffset := clampCatalogPage(tt.limit, tt.offset)
			if gotLimit != tt.wantLimit {
				t.Errorf("clampCatalogPage(%d, %d) limit = %d, want %d", tt.limit, tt.offset, gotLimit, tt.wantLimit)
			}
			if gotOffset != tt.wantOffset {
				t.Errorf("clampCatalogPage(%d, %d) offset = %d, want %d", tt.limit, tt.offset, gotOffset, tt.wantOffset)
			}
		})
	}
}
