package stripemock

import "net/url"

// invoiceRead serves the retained billing history, not merely latest_invoice.
// All lists use the same stable cursor and created filters as money history.
func (m *Mock) invoiceRead(seg []string, query url.Values) (int, any) {
	matching := map[string]Object{}
	if seg[0] == "invoice_payments" {
		if len(seg) != 1 {
			return 404, stripeErr("resource_missing")
		}
		for id, invoice := range m.invoices {
			if wanted := query.Get("invoice"); wanted != "" && id != wanted {
				continue
			}
			for _, payment := range invoice["payments"].(Object)["data"].([]Object) {
				matching[payment["id"].(string)] = payment
			}
		}
		return 200, stripeList(matching, query)
	}
	if len(seg) == 1 {
		for id, invoice := range m.invoices {
			sub := invoice["parent"].(Object)["subscription_details"].(Object)["subscription"]
			if wanted := query.Get("subscription"); wanted != "" && sub != wanted {
				continue
			}
			if status := query.Get("status"); status != "" && invoice["status"] != status {
				continue
			}
			matching[id] = invoice
		}
		return 200, stripeList(matching, query)
	}
	invoice, ok := m.invoices[seg[1]]
	if !ok {
		return 404, stripeErr("resource_missing")
	}
	if len(seg) == 2 {
		return 200, invoice
	}
	if len(seg) == 3 && seg[2] == "lines" {
		for _, line := range invoice["lines"].(Object)["data"].([]Object) {
			matching[line["id"].(string)] = line
		}
		return 200, stripeList(matching, query)
	}
	return 404, stripeErr("resource_missing")
}
