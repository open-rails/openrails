package routes

// linkedCustomerOperations is the existing linked-customer action policy. Both
// the old network mount and generic host mapping use this one classification.
var linkedCustomerOperations = []struct {
	key       string
	sensitive bool
}{
	{"GET /v1/me/subscriptions/{id}", false},
	{"POST /v1/me/subscriptions/{id}/cancel", true},
	{"POST /v1/me/subscriptions/{id}/resume", true},
	{"PUT /v1/me/subscriptions/{id}/payment-method", true},
	{"GET /v1/me/payment-methods", false},
	{"POST /v1/me/payment-methods", true},
	{"DELETE /v1/me/payment-methods/{id}", true},
	{"GET /v1/me/orders/{id}", false},
	{"POST /v1/me/orders/{id}/pay", true},
	{"POST /v1/me/orders/{id}/confirm", true},
	{"GET /v1/me/agreements", false},
	{"POST /v1/me/agreements/{id}/revoke", true},
}

func mappedCustomerOperation(key string) (sensitive, allowed bool) {
	if key == "GET /v1/me/orders" || key == "GET /v1/me/subscriptions" {
		return false, true
	}
	for _, operation := range linkedCustomerOperations {
		if operation.key == key {
			return operation.sensitive, true
		}
	}
	return false, false
}
