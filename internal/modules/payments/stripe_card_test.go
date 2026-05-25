package payments

import "testing"

func TestNormalizeStripeCard(t *testing.T) {
	t.Run("nil when no last4", func(t *testing.T) {
		if got := NormalizeStripeCard(StripeCardDetails{Brand: "visa"}); got != nil {
			t.Fatalf("expected nil card when last4 missing, got %+v", got)
		}
	})

	t.Run("normalizes brand, last4 and expiry", func(t *testing.T) {
		got := NormalizeStripeCard(StripeCardDetails{Brand: "visa", Last4: "0137", ExpMonth: 3, ExpYear: 2027})
		if got == nil {
			t.Fatal("expected card, got nil")
		}
		if got.Brand != "Visa" {
			t.Errorf("brand = %q, want Visa", got.Brand)
		}
		if got.Last4 != "0137" {
			t.Errorf("last4 = %q, want 0137", got.Last4)
		}
		if got.Expiry != "03/27" {
			t.Errorf("expiry = %q, want 03/27", got.Expiry)
		}
	})

	t.Run("empty expiry when exp unknown", func(t *testing.T) {
		got := NormalizeStripeCard(StripeCardDetails{Brand: "amex", Last4: "1001"})
		if got == nil || got.Expiry != "" {
			t.Fatalf("expected empty expiry, got %+v", got)
		}
	})
}

func TestTitleCaseBrand(t *testing.T) {
	cases := map[string]string{
		"visa":       "Visa",
		"mastercard": "Mastercard",
		"MasterCard": "MasterCard",
		"":           "",
		"  amex":     "Amex",
	}
	for in, want := range cases {
		if got := TitleCaseBrand(in); got != want {
			t.Errorf("TitleCaseBrand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompactStrings(t *testing.T) {
	got := compactStrings(" ch_1 ", "", "pi_1", "ch_1", " in_1 ")
	want := []string{"ch_1", "pi_1", "in_1"}
	if len(got) != len(want) {
		t.Fatalf("compactStrings = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("compactStrings = %v, want %v", got, want)
		}
	}
}
