package routes

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

const selectorTestUserID = "11111111-1111-1111-1111-111111111111"

type merchantSelectorAuth struct {
	merchant string
}

func (a merchantSelectorAuth) Authenticate(_ context.Context, _ *http.Request) (billingauth.UserContext, error) {
	return billingauth.UserContext{UserID: selectorTestUserID, Merchant: a.merchant}, nil
}

type merchantSelectorChecker struct {
	allowed       map[string]bool
	merchantIDs   map[string]merchant.ID
	inferred      string
	inferErr      error
	inferenceRuns int
}

func (c *merchantSelectorChecker) ResolveAuthorizedMerchant(_ context.Context, merchantRef, userID, _ string) (merchant.ID, string, error) {
	if merchantRef == "" {
		c.inferenceRuns++
		if c.inferErr != nil || c.inferred == "" {
			return merchant.ID{}, "", policy.ErrMerchantUnresolved
		}
		merchantRef = c.inferred
	}
	if userID != selectorTestUserID || !c.allowed[merchantRef] {
		return merchant.ID{}, "", policy.ErrPermissionRequired
	}
	id, ok := c.merchantIDs[merchantRef]
	if !ok {
		return merchant.ID{}, "", policy.ErrMerchantUnresolved
	}
	return id, merchantRef, nil
}

func TestMerchantSelector(t *testing.T) {
	merchantA := merchant.ID(uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"))
	merchantB := merchant.ID(uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"))

	tests := []struct {
		name              string
		authMerchant      string
		selector          string
		allowed           map[string]bool
		merchantIDs       map[string]merchant.ID
		inferred          string
		inferErr          error
		wantMerchant      string
		wantMerchantID    merchant.ID
		wantMessage       string
		wantInferenceRun  int
		hostMerchantID    merchant.ID
		contextMerchantID merchant.ID
	}{
		{
			name:           "explicit selection",
			selector:       " merchant-b ",
			allowed:        map[string]bool{"merchant-b": true},
			merchantIDs:    map[string]merchant.ID{"merchant-b": merchantB},
			wantMerchant:   "merchant-b",
			wantMerchantID: merchantB,
		},
		{
			name:             "single merchant inference remains",
			allowed:          map[string]bool{"merchant-a": true},
			merchantIDs:      map[string]merchant.ID{"merchant-a": merchantA},
			inferred:         "merchant-a",
			wantMerchant:     "merchant-a",
			wantMerchantID:   merchantA,
			wantInferenceRun: 1,
		},
		{
			name:             "ambiguous membership without selector",
			allowed:          map[string]bool{},
			merchantIDs:      map[string]merchant.ID{},
			inferErr:         errors.New("multiple merchant memberships"),
			wantMessage:      "merchant_unresolved",
			wantInferenceRun: 1,
		},
		{
			name:        "selected merchant requires live permission",
			selector:    "merchant-b",
			allowed:     map[string]bool{},
			merchantIDs: map[string]merchant.ID{"merchant-b": merchantB},
			wantMessage: "permission_required",
		},
		{
			name:        "selected merchant must resolve active",
			selector:    "merchant-b",
			allowed:     map[string]bool{"merchant-b": true},
			merchantIDs: map[string]merchant.ID{},
			wantMessage: "merchant_unresolved",
		},
		{
			name:           "authenticated merchant cannot be overridden",
			authMerchant:   "merchant-a",
			selector:       "merchant-b",
			allowed:        map[string]bool{"merchant-a": true, "merchant-b": true},
			merchantIDs:    map[string]merchant.ID{"merchant-a": merchantA, "merchant-b": merchantB},
			wantMerchant:   "merchant-a",
			wantMerchantID: merchantA,
		},
		{
			name:           "selected merchant must match host",
			selector:       "merchant-a",
			allowed:        map[string]bool{"merchant-a": true},
			merchantIDs:    map[string]merchant.ID{"merchant-a": merchantA},
			hostMerchantID: merchantB,
			wantMessage:    "host_merchant_mismatch",
		},
		{
			name:              "selected merchant must match existing context",
			selector:          "merchant-a",
			allowed:           map[string]bool{"merchant-a": true},
			merchantIDs:       map[string]merchant.ID{"merchant-a": merchantA},
			contextMerchantID: merchantB,
			wantMessage:       "merchant_context_mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker := &merchantSelectorChecker{
				allowed:     tt.allowed,
				merchantIDs: tt.merchantIDs,
				inferred:    tt.inferred,
				inferErr:    tt.inferErr,
			}
			gate := NewGate(GateOptions{
				Authenticator:          merchantSelectorAuth{merchant: tt.authMerchant},
				AdminPermissionChecker: checker,
			})
			req, err := http.NewRequest(http.MethodGet, "/v1/merchant/settings", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			if tt.selector != "" {
				req.Header.Set(billingauth.MerchantSelectorHeader, tt.selector)
			}

			ctx := t.Context()
			if tt.contextMerchantID != (merchant.ID{}) {
				ctx = merchant.WithID(ctx, tt.contextMerchantID)
			}
			if tt.hostMerchantID != (merchant.ID{}) {
				ctx = merchant.WithHostMerchant(ctx, tt.hostMerchantID)
			}
			principal, err := gate.Authorize(ctx, req, "merchant:settings:read")
			if tt.wantMessage != "" {
				var gateErr billingauth.GateError
				if !errors.As(err, &gateErr) {
					t.Fatalf("Authorize() error = %v, want GateError %q", err, tt.wantMessage)
				}
				if gateErr.Status != http.StatusForbidden || gateErr.Message != tt.wantMessage {
					t.Fatalf("Authorize() GateError = (%d, %q), want (403, %q)", gateErr.Status, gateErr.Message, tt.wantMessage)
				}
			} else {
				if err != nil {
					t.Fatalf("Authorize(): %v", err)
				}
				if principal.UserContext.Merchant != tt.wantMerchant {
					t.Fatalf("Authorize() merchant = %q, want %q", principal.UserContext.Merchant, tt.wantMerchant)
				}
				if principal.MerchantID != tt.wantMerchantID {
					t.Fatalf("Authorize() merchant ID = %s, want %s", principal.MerchantID, tt.wantMerchantID)
				}
			}
			if checker.inferenceRuns != tt.wantInferenceRun {
				t.Fatalf("merchant inference calls = %d, want %d", checker.inferenceRuns, tt.wantInferenceRun)
			}
		})
	}
}

func TestMerchantSelectorNilRequestUsesMembershipInference(t *testing.T) {
	merchantA := merchant.ID(uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"))
	checker := &merchantSelectorChecker{
		allowed:     map[string]bool{"merchant-a": true},
		merchantIDs: map[string]merchant.ID{"merchant-a": merchantA},
		inferred:    "merchant-a",
	}
	gate := NewGate(GateOptions{
		Authenticator:          merchantSelectorAuth{},
		AdminPermissionChecker: checker,
	})

	principal, err := gate.Authorize(t.Context(), nil, "merchant:settings:read")
	if err != nil {
		t.Fatalf("Authorize(): %v", err)
	}
	if principal.MerchantID != merchantA || principal.UserContext.Merchant != "merchant-a" {
		t.Fatalf("Authorize() principal = %+v, want inferred merchant-a", principal)
	}
}
