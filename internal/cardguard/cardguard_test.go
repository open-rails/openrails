package cardguard

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Published network test PANs across every brand, length and IIN range.
var testPANs = []string{
	"4111111111111111", "4012888888881881", "4222222222222", "4006000000000000003",
	"5555555555554444", "5105105105105100", "2223003122003222",
	"378282246310005", "371449635398431", "6011111111111117", "6011000990139424",
	"30569309025904", "38520000023237", "3530111333300000", "3566002020360505", "6200000000000005",
}

func groupsOf(digits, sep string, sizes ...int) string {
	var parts []string
	for _, n := range sizes {
		parts, digits = append(parts, digits[:n]), digits[n:]
	}
	for len(sizes) == 0 && len(digits) > 4 {
		parts, digits = append(parts, digits[:4]), digits[4:]
	}
	return strings.Join(append(parts, digits), sep)
}

// SAQ A firewall (#795 B5): every shape a human or form writes a card in is refused.
func TestContainsPANRefusesCardShapes(t *testing.T) {
	for _, pan := range testPANs {
		spaced := groupsOf(pan, " ")
		for _, v := range []string{
			pan, spaced, groupsOf(pan, "-"), "please charge " + spaced + " today",
			`{"note":"` + pan + `"}`, spaced + " 12 29", "88 " + spaced, "ref" + pan + "x",
		} {
			require.Truef(t, ContainsPAN(v), "%q must be refused", v)
		}
	}
	for _, v := range []string{
		groupsOf("378282246310005", " ", 4, 6), groupsOf("378282246310005", "-", 4, 6), // Amex 4-6-5
		groupsOf("30569309025904", " ", 4, 6), groupsOf("30569309025904", "-", 4, 6), // Diners 4-6-4
	} {
		require.Truef(t, ContainsPAN(v), "%q must be refused", v)
	}
	for _, pan := range testPANs {
		digits := []byte(pan)
		for i := range digits {
			digits[i] -= '0'
		}
		require.Truef(t, luhnValid(digits) && issuedRange(digits), "%s must be Luhn-valid and in an issued range", pan)
	}
}

func TestContainsPANGroupingRules(t *testing.T) {
	for v, want := range map[string]bool{
		"":                               false,
		"4111":                           false,
		"41111111111111111111":           false, // 20 digits
		"4111111111111112":               false, // Luhn
		"4111  1111  1111  1111":         true,
		"4111 - 1111 - 1111 - 1111":      true,
		"4111-1111-1111-116":             true, // short tail
		"4111--1111--1111--1111":         false,
		"4111.1111.1111.1111":            false,
		"4111/1111/1111/1111":            false,
		"41111111-11111111":              false, // 8-8
		"4111111111-111111":              false, // 10-6
		"411-111-111-111-1111":           false, // 3-3-3-3-4
		"x4111-1111-1111-1111":           false, // separated and glued to a letter
		"1758153600123456786":            false, // epoch ns, Luhn-valid
		"order-1758153600123456786":      false,
		"1758153600123458":               false, // epoch us
		"1758153600129":                  false, // epoch ms
		"9999999999999995":               false, // MII 9
		"0000000000000000":               false,
		"2026-09-18T00:00:00.123456789Z": false,
	} {
		require.Equalf(t, want, ContainsPAN(v), "%q", v)
	}
}

// UUIDs whose digit groups form a Luhn-valid run once dashes read as card
// formatting; each was refused before the grouping rule. Fixed regression corpus.
var luhnTrippingUUIDs = []string{
	"a544fda7-1958-4199-9417-3263a6c4b369", "463d4942-14ef-4eec-9436-151088257692",
	"a9ac5110-4c89-4917-8028-24049a3a1644", "40ac552b-c48b-45fc-8968-269009312aae",
	"805b6084-0619-4770-9a20-90104f447f64", "a1064dc7-33e4-4fff-9023-371564343050",
	"bc04e817-d521-4287-8039-53f17efdd48c", "0d9615bb-7353-4c29-8689-79388540f9ba",
	"57f992de-0186-4965-9165-9e47e4ac7858", "b93b6842-1499-4083-905d-db0ce15cd21c",
	"c3352a65-0139-4452-8285-a29e2c168c3b", "131c0764-7005-4133-8bab-b8d7cdc76785",
	"12057668-0451-4682-adb2-67566374b569", "bd229d93-4912-4074-8110-e72fe353bbc6",
	"93adc553-9787-4312-907d-6d6d787b7cf6", "45557640-6562-41cc-b7af-995cc3aff45e",
	"bfb5d852-d7c7-4116-8493-7503b9db92e9", "cf18c2f4-3133-4788-8680-d05dbcf14a78",
	"f9c10cb2-f678-4727-9130-4916139d86c5", "3ce00077-0322-4993-bdff-43c8d74a9c4d",
	"ac8f50f5-4c07-4270-9835-703e37a801a3", "efb3b394-3996-4367-94c8-af857ffaef4a",
	"756de35f-ce3a-4830-8848-1143635d697b", "3b97792a-f73b-4006-9855-95524388e149",
	"abcdefab-cdef-4abc-8111-111111111112", "a4111111-1111-4111-8119-abcdefabcdef",
}

var typedIDPrefixes = []string{"", "sub_", "price_", "pay_", "pm_", "prod_", "cs_", "in_", "chk_"}

func acceptsIdentifier(t *testing.T, id string) {
	t.Helper()
	for _, prefix := range typedIDPrefixes {
		for _, v := range []string{prefix + id, prefix + strings.ToUpper(id), `{"idempotency_key":"` + prefix + id + `"}`} {
			if ContainsPAN(v) {
				t.Fatalf("identifier %q refused as a card number", v)
			}
		}
	}
}

// A uuid.NewString() Idempotency-Key was once refused ~1 in 480 times.
func TestContainsPANAcceptsStructuredIdentifiers(t *testing.T) {
	for _, id := range luhnTrippingUUIDs {
		acceptsIdentifier(t, id)
	}
	for range 20_000 {
		acceptsIdentifier(t, uuid.NewString())
	}
}

func FuzzContainsPANAcceptsUUIDs(f *testing.F) {
	for _, seed := range luhnTrippingUUIDs {
		id := uuid.MustParse(seed)
		f.Add(id[:])
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		var b [16]byte
		copy(b[:], raw)
		b[6] = (b[6] & 0x0f) | 0x40
		b[8] = (b[8] & 0x3f) | 0x80
		acceptsIdentifier(t, uuid.UUID(b).String())
	})
}
