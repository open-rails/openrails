package openrails

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestProviderBillingQualificationWireContract(t *testing.T) {
	when := time.Date(2026, 9, 16, 12, 0, 0, 123456000, time.UTC)
	cost, rated := int64(math.MaxInt64), int64(math.MaxInt64)
	body := []byte(`{"contract":"openrails/pass-through-provider-cost"}`)
	digest := SHA256(sha256.Sum256(body))
	value := ProviderBillingQualification{
		OperationID: "rental/create",
		MerchantID:  uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Lifecycle: ProviderBillingLifecycleEvidence{
			Provider: "runpod", ProviderResourceID: "pod-1",
			ProviderLifetimeStart: when.Add(-2 * time.Hour), ProviderLifetimeEnd: when.Add(-time.Hour),
			ProviderAbsentAt: when, ProviderAbsenceReference: "absence:1", BillingStopReference: "stop:1",
			WindowsClosedAt: when, WindowsClosedReference: "windows:1", LifecycleEvidenceBody: []byte(`{}`),
		},
		LifecycleEvidenceSHA256: SHA256(sha256.Sum256([]byte(`{}`))),
		QuiescenceSeconds:       86400,
		State:                   ProviderBillingQualificationEligible,
		Reason:                  ProviderBillingEligible,
		BaselineObservationID:   "obs-1", QualifiedObservationID: "obs-2",
		QualifiedProviderCostUSDMicros: &cost,
		QualifiedAt:                    &when,
		Authorization: OperationAuthorization{
			OperationID: "rental/create", MerchantID: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
			Payer: CustomerID(uuid.MustParse("22222222-2222-2222-2222-222222222222")), RecordOwner: "user:1",
			AuthorizedUSDMicros: math.MaxInt64, ClaimReference: "claim:1", AuthorizationBody: []byte(`{"op":1}`),
			AuthorizationBodySHA256: SHA256(sha256.Sum256([]byte(`{"op":1}`))), State: OperationAuthorizationSettled,
			TerminalReference: "sha256:" + digest.String(), SettlementProviderCostUSDMicros: &cost,
			SettlementRatedUSDMicros: &rated, SettlementBody: body, SettlementBodySHA256: &digest,
			CreatedAt: when, SettledAt: &when,
		},
		CreatedAt: when, UpdatedAt: when,
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("testdata/wire/provider_billing_qualification.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != strings.TrimSpace(string(fixture)) {
		t.Fatalf("provider billing qualification wire changed:\n%s", raw)
	}
	var got ProviderBillingQualification
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(value, got) {
		t.Fatalf("qualification lost precision or null semantics: %#v", got)
	}

	open := OperationAuthorization{State: OperationAuthorizationOpen, CreatedAt: when}
	raw, err = json.Marshal(open)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"settlement_rated_usd_micros":null`, `"settlement_body":null`, `"settlement_body_sha256":null`} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("unsettled authorization must encode %s: %s", field, raw)
		}
	}
	var openGot OperationAuthorization
	if err := json.Unmarshal(raw, &openGot); err != nil || !reflect.DeepEqual(open, openGot) {
		t.Fatalf("unsettled authorization round trip: %v %#v", err, openGot)
	}
}

func TestSHA256WireIsCanonicalHex(t *testing.T) {
	var digest SHA256
	valid := strings.Repeat("ab", sha256.Size)
	if err := digest.UnmarshalText([]byte(valid)); err != nil || digest.String() != valid {
		t.Fatalf("valid digest: %v %s", err, digest)
	}
	for _, invalid := range []string{strings.ToUpper(valid), valid[:62], valid + "00", strings.Repeat("zz", sha256.Size)} {
		if err := digest.UnmarshalText([]byte(invalid)); err == nil {
			t.Fatalf("digest %q must be refused", invalid)
		}
	}
}

// The public commands carry facts, never a caller-rated or settlement amount.
func TestProviderObligationRequestsCarryNoRatedAmount(t *testing.T) {
	var walk func(reflect.Type, string)
	walk = func(typ reflect.Type, path string) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || typ.PkgPath() != reflect.TypeOf(Client{}).PkgPath() {
			return
		}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := strings.ToLower(field.Name + " " + field.Tag.Get("json"))
			if strings.Contains(name, "rated") || strings.Contains(name, "settle") || strings.Contains(name, "charge") {
				t.Errorf("%s.%s lets a caller supply a settlement amount", path, field.Name)
			}
			walk(field.Type, path+"."+field.Name)
		}
	}
	for _, request := range []any{OperationAuthorizationRequest{}, ReleaseOperationAuthorizationRequest{}, ProviderBillingObservationRequest{}} {
		walk(reflect.TypeOf(request), reflect.TypeOf(request).Name())
	}
}

func TestProviderObligationErrorsClassifyAcrossTransports(t *testing.T) {
	cases := []struct {
		sentinel error
		status   int
		class    error
	}{
		{ErrOperationAuthorizationNotFound, http.StatusNotFound, ErrNotFound},
		{ErrProviderBillingQualificationNotFound, http.StatusNotFound, ErrNotFound},
		{ErrOperationAuthorizationConflict, http.StatusConflict, ErrConflict},
		{ErrOperationAuthorizationNotOpen, http.StatusConflict, ErrConflict},
		{ErrOperationAuthorizationHasBillingEvidence, http.StatusConflict, ErrConflict},
		{ErrProviderBillingObservationConflict, http.StatusConflict, ErrConflict},
		{ErrProviderBillingQualificationRefused, http.StatusConflict, ErrConflict},
	}
	for _, tc := range cases {
		remote := &StatusError{Status: tc.status, ErrorDetails: ErrorDetails{Code: tc.sentinel.Error()}}
		for _, err := range []error{tc.sentinel, remote} {
			if !errors.Is(err, tc.sentinel) || !errors.Is(err, tc.class) {
				t.Fatalf("%v must match %v and its class %v", err, tc.sentinel, tc.class)
			}
			if errors.Is(err, ErrInvalid) || errors.Is(err, ErrInternal) {
				t.Fatalf("%v matched the wrong class", err)
			}
		}
		for _, other := range cases {
			if other.sentinel != tc.sentinel && errors.Is(remote, other.sentinel) {
				t.Fatalf("code %s matched %v", tc.sentinel, other.sentinel)
			}
		}
	}
	conflict := error(&OperationAuthorizationConflict{Field: "authorization_body"})
	if !errors.Is(conflict, ErrOperationAuthorizationConflict) || !errors.Is(conflict, ErrConflict) {
		t.Fatal("typed conflict must keep its sentinel and class")
	}
}

func TestProviderOperationPathIsOneSegment(t *testing.T) {
	path, err := providerOperationPath("rental/1?#%/create")
	if err != nil || path != "/v1/merchant/provider-operations/rental%2F1%3F%23%25%2Fcreate" {
		t.Fatalf("escaped path: %q %v", path, err)
	}
	for _, id := range []string{"", ".", ".."} {
		if _, err := providerOperationPath(id); !errors.Is(err, ErrInvalid) {
			t.Fatalf("operation id %q must be invalid, got %v", id, err)
		}
	}
}
