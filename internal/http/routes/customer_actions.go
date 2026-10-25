package routes

// mappedCustomerOperations explicitly classifies the customer operations a
// host may mount with its authenticated customer mapping.
var mappedCustomerOperations = []struct {
	key       string
	sensitive bool
}{
	{"GET /v1/me/config", false},
	{"GET /v1/me/payments", false},
	{"GET /v1/me/entitlements", false},
	{"GET /v1/me/subscriptions/{id}", false},
	{"POST /v1/me/subscriptions/{id}/cancel", true},
	{"POST /v1/me/subscriptions/{id}/resume", true},
	{"PUT /v1/me/subscriptions/{id}/payment-method", true},
	{"GET /v1/me/payment-methods", false},
	{"POST /v1/me/payment-methods", true},
	{"POST /v1/me/payment-methods/{id}/confirm", true},
	{"DELETE /v1/me/payment-methods/{id}", true},
	{"GET /v1/me/orders/{id}", false},
	{"GET /v1/me/orders/{id}/payment-attempt", false},
	{"POST /v1/me/orders/{id}/pay", true},
	{"POST /v1/me/orders/{id}/confirm", true},
	{"GET /v1/me/agreements", false},
	{"POST /v1/me/agreements/{id}/revoke", true},
}

func mappedCustomerOperation(key string) (sensitive, allowed bool) {
	if key == "GET /v1/me/orders" || key == "GET /v1/me/subscriptions" {
		return false, true
	}
	for _, operation := range mappedCustomerOperations {
		if operation.key == key {
			return operation.sensitive, true
		}
	}
	return false, false
}
