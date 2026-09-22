package handlers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/stretchr/testify/require"
)

func TestPricedCheckoutRejectsOperationSelectors(t *testing.T) {
	for _, field := range []string{"mode", "subscription_id", "new_price_id"} {
		t.Run(field, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/merchant/checkout-sessions", strings.NewReader(fmt.Sprintf(`{"%s":"one_off"}`, field)))
			recorder := httptest.NewRecorder()
			ServiceCreateCheckoutSession(httprequest.NewHTTP(recorder, req, nil))
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Contains(t, recorder.Body.String(), "dedicated setup or subscription action")
		})
	}
}
