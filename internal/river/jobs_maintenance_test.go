package riverjobs

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/integrations/ccbill"
)

func TestIsCCBillDataLinkActive(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{status: "1", want: true},
		{status: "Y", want: true},
		{status: "active", want: true},
		{status: "0", want: false},
		{status: "cancelled", want: false},
		{status: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			if got := ccbill.IsDataLinkActiveStatus(tt.status); got != tt.want {
				t.Fatalf("expected %v, got %v", tt.want, got)
			}
		})
	}
}

func TestCCBillDataLinkPaidThrough(t *testing.T) {
	paidThrough, ok := ccbill.DataLinkPaidThrough(ccbill.CCBillRecord{ExpiryDate: "2099-06-03"}, time.Now().UTC())
	if !ok {
		t.Fatal("expected future expiry date to parse")
	}
	if paidThrough.Hour() != 23 || paidThrough.Minute() != 59 || paidThrough.Second() != 59 {
		t.Fatalf("expected end-of-day paid through, got %v", paidThrough)
	}

	_, ok = ccbill.DataLinkPaidThrough(ccbill.CCBillRecord{ExpiryDate: "2000-01-01", RebillDate: ""}, time.Now().UTC())
	if ok {
		t.Fatal("expected past date to be rejected")
	}
}
