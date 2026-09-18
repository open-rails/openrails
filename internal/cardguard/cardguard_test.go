package cardguard

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Published network test PANs. Every one of these must be refused in every
// shape a human or a form ever writes a card in.
var testPANs = []struct {
	brand  string
	number string
}{
	{"visa-16", "4111111111111111"},
	{"visa-16-alt", "4012888888881881"},
	{"visa-13", "4222222222222"},
	{"visa-19", "4006000000000000003"},
	{"mastercard-16", "5555555555554444"},
	{"mastercard-16-alt", "5105105105105100"},
	{"mastercard-2series", "2223003122003222"},
	{"amex-15", "378282246310005"},
	{"amex-15-alt", "371449635398431"},
	{"discover-16", "6011111111111117"},
	{"discover-16-alt", "6011000990139424"},
	{"diners-14", "30569309025904"},
	{"diners-14-alt", "38520000023237"},
	{"jcb-16", "3530111333300000"},
	{"jcb-16-alt", "3566002020360505"},
	{"unionpay-16", "6200000000000005"},
}

// group splits digits into the given group sizes, joined by sep.
func group(digits string, sep string, sizes ...int) string {
	var parts []string
	for _, n := range sizes {
		parts, digits = append(parts, digits[:n]), digits[n:]
	}
	if digits != "" {
		parts = append(parts, digits)
	}
	return strings.Join(parts, sep)
}

// groupsOfFour is how every issuer and every checkout form prints a card.
func groupsOfFour(digits string, sep string) string {
	var parts []string
	for len(digits) > 4 {
		parts, digits = append(parts, digits[:4]), digits[4:]
	}
	return strings.Join(append(parts, digits), sep)
}

func TestContainsPANRefusesRealCardShapes(t *testing.T) {
	t.Parallel()
	for _, pan := range testPANs {
		for _, shape := range []struct{ name, value string }{
			{"plain", pan.number},
			{"spaced", groupsOfFour(pan.number, " ")},
			{"dashed", groupsOfFour(pan.number, "-")},
			{"in a sentence", "please charge " + groupsOfFour(pan.number, " ") + " today"},
			{"quoted in a json body", `{"note":"` + pan.number + `"}`},
			{"trailing expiry digits", groupsOfFour(pan.number, " ") + " 12 29"},
			{"leading order digits", "88 " + groupsOfFour(pan.number, " ")},
		} {
			require.Truef(t, ContainsPAN(shape.value), "%s %s: %q must be refused", pan.brand, shape.name, shape.value)
		}
	}

	// The two non-uniform groupings issuers actually print.
	require.True(t, ContainsPAN(group("378282246310005", " ", 4, 6)), "amex 4-6-5 spaced")
	require.True(t, ContainsPAN(group("378282246310005", "-", 4, 6)), "amex 4-6-5 dashed")
	require.True(t, ContainsPAN(group("30569309025904", " ", 4, 6)), "diners 4-6-4 spaced")
	require.True(t, ContainsPAN(group("30569309025904", "-", 4, 6)), "diners 4-6-4 dashed")
}

// Pinned v4 UUIDs whose digit groups form a Luhn-valid 13-19 digit run once the
// dashes are read as card formatting. Every one of them was refused before the
// grouping rule landed; generated from a seeded PRNG so the table is a fixed
// regression corpus, not a lucky draw.
var luhnTrippingUUIDs = []string{
	"a544fda7-1958-4199-9417-3263a6c4b369",
	"463d4942-14ef-4eec-9436-151088257692",
	"a9ac5110-4c89-4917-8028-24049a3a1644",
	"40ac552b-c48b-45fc-8968-269009312aae",
	"805b6084-0619-4770-9a20-90104f447f64",
	"a1064dc7-33e4-4fff-9023-371564343050",
	"bc04e817-d521-4287-8039-53f17efdd48c",
	"0d9615bb-7353-4c29-8689-79388540f9ba",
	"57f992de-0186-4965-9165-9e47e4ac7858",
	"b93b6842-1499-4083-905d-db0ce15cd21c",
	"c3352a65-0139-4452-8285-a29e2c168c3b",
	"131c0764-7005-4133-8bab-b8d7cdc76785",
	"12057668-0451-4682-adb2-67566374b569",
	"bd229d93-4912-4074-8110-e72fe353bbc6",
	"93adc553-9787-4312-907d-6d6d787b7cf6",
	"45557640-6562-41cc-b7af-995cc3aff45e",
	"bfb5d852-d7c7-4116-8493-7503b9db92e9",
	"cf18c2f4-3133-4788-8680-d05dbcf14a78",
	"f9c10cb2-f678-4727-9130-4916139d86c5",
	"3ce00077-0322-4993-bdff-43c8d74a9c4d",
	"ac8f50f5-4c07-4270-9835-703e37a801a3",
	"efb3b394-3996-4367-94c8-af857ffaef4a",
	"756de35f-ce3a-4830-8848-1143635d697b",
	"3b97792a-f73b-4006-9855-95524388e149",
	// The two fixtures earlier per-field exemptions were added for.
	"abcdefab-cdef-4abc-8111-111111111112",
	"a4111111-1111-4111-8119-abcdefabcdef",
}

// Typed id prefixes the API spells identifiers with (#470/#485).
var typedIDPrefixes = []string{"sub_", "price_", "pay_", "pm_", "prod_", "cs_", "in_", "chk_"}

func TestContainsPANAcceptsStructuredIdentifiers(t *testing.T) {
	t.Parallel()
	for _, id := range luhnTrippingUUIDs {
		require.Falsef(t, ContainsPAN(id), "UUID %s is an identifier, not a card number", id)
		require.Falsef(t, ContainsPAN(strings.ToUpper(id)), "upper-case UUID %s", id)
		for _, prefix := range typedIDPrefixes {
			require.Falsef(t, ContainsPAN(prefix+id), "typed id %s%s", prefix, id)
		}
		require.Falsef(t, ContainsPAN(`{"idempotency_key":"`+id+`"}`), "UUID %s inside a json body", id)
	}

	// Other opaque client keys that Luhn alone refuses.
	for _, key := range []string{
		"1758153600123456786",            // epoch nanoseconds, Luhn-valid
		"order-1758153600123456786",      // ... behind a client prefix
		"1758153600123458",               // epoch microseconds, Luhn-valid
		"1758153600129",                  // epoch milliseconds, Luhn-valid
		"9999999999999995",               // MII 9 is not issued as a card account
		"0000000000000000",               // all zeros passes Luhn
		"2026-09-18T00:00:00.123456789Z", // an RFC3339 nanosecond stamp
	} {
		require.Falsef(t, ContainsPAN(key), "opaque key %q is not a card number", key)
	}
}

// The measured regression: a bare uuid.NewString() used as an Idempotency-Key
// was refused ~1 in 480 times (417/200000 at origin/master 2a28bcab1).
func TestContainsPANNeverRefusesRandomUUIDKeys(t *testing.T) {
	t.Parallel()
	const samples = 200_000
	refused := 0
	for i := 0; i < samples; i++ {
		id := uuid.NewString()
		if ContainsPAN(id) {
			refused++
			t.Errorf("random UUID key refused: %s", id)
		}
		for _, prefix := range typedIDPrefixes[:6] {
			if ContainsPAN(prefix + id) {
				refused++
				t.Errorf("random typed id refused: %s%s", prefix, id)
			}
		}
		if refused > 10 {
			t.Fatal("too many refusals; stopping")
		}
	}
	require.Zerof(t, refused, "%d/%d random UUID idempotency keys refused", refused, samples)
}

// FuzzContainsPANAcceptsUUIDs mutates the 16 bytes behind a v4 UUID, so the
// fuzzer explores the digit/hex layouts the property test samples randomly.
func FuzzContainsPANAcceptsUUIDs(f *testing.F) {
	f.Add(make([]byte, 16))
	for _, seed := range luhnTrippingUUIDs {
		id := uuid.MustParse(seed)
		f.Add(id[:])
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) < 16 {
			return
		}
		var b [16]byte
		copy(b[:], raw)
		b[6] = (b[6] & 0x0f) | 0x40
		b[8] = (b[8] & 0x3f) | 0x80
		id := uuid.UUID(b).String()
		if ContainsPAN(id) {
			t.Fatalf("UUID %s refused as a card number", id)
		}
		for _, prefix := range typedIDPrefixes {
			if ContainsPAN(prefix + id) {
				t.Fatalf("typed id %s%s refused as a card number", prefix, id)
			}
		}
	})
}

func TestContainsPANGroupingRules(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value string
		want  bool
		why   string
	}{
		{"", false, "empty"},
		{"4111", false, "too short"},
		{"41111111111111111111", false, "20 digits is longer than any PAN"},
		{"4111111111111112", false, "fails Luhn"},
		{"4111-1111-1111-1111", true, "dashed groups of four"},
		{"4111 1111 1111 1111", true, "spaced groups of four"},
		{"4111  1111  1111  1111", true, "wide spacing is still card formatting"},
		{"4111 - 1111 - 1111 - 1111", true, "one dash per gap is still card formatting"},
		{"4111--1111--1111--1111", false, "a double dash is not how a card is written"},
		{"4111.1111.1111.1111", false, "dots are not card formatting"},
		{"4111/1111/1111/1111", false, "slashes are not card formatting"},
		{"41111111-11111111", false, "8-8 is not a card grouping"},
		{"4111111111-111111", false, "10-6 is not a card grouping"},
		{"411-111-111-111-1111", false, "3-3-3-3-4 is not a card grouping"},
		{"4111-1111-1111-116", true, "a short tail is a card grouping"},
	} {
		require.Equalf(t, tc.want, ContainsPAN(tc.value), "%q: %s", tc.value, tc.why)
	}
}

func TestIssuedRangeCoversEveryNetworkTestPAN(t *testing.T) {
	t.Parallel()
	for _, pan := range testPANs {
		digits := make([]byte, len(pan.number))
		for i := range digits {
			digits[i] = pan.number[i] - '0'
		}
		require.Truef(t, luhnValid(digits), "%s test PAN must pass Luhn", pan.brand)
		require.Truef(t, issuedRange(digits), "%s (%s) must be inside an issued range", pan.brand, pan.number)
	}
}

func BenchmarkContainsPANUUID(b *testing.B) {
	id := uuid.NewString()
	for i := 0; i < b.N; i++ {
		if ContainsPAN(id) {
			b.Fatal(fmt.Sprintf("unexpected refusal for %s", id))
		}
	}
}
