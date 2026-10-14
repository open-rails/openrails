package stripemock

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// legacyAmountLocked is a legacy price's amount: 9.99 unless SetLegacyPrice set it.
func (m *Mock) legacyAmountLocked(price string) int64 {
	if amount, ok := m.amounts[price]; ok {
		return amount
	}
	return 999
}

// changeItemLocked moves the subscription's item to another price. With
// billing_cycle_anchor=now the cycle restarts and the proration is invoiced
// and charged at once; error_if_incomplete refuses a declined charge. A zero
// status means the change applied.
func (m *Mock) changeItemLocked(s Object, form url.Values) (int, any) {
	item := s["items"].(Object)["data"].([]Object)[0]
	if form.Get("items[0][id]") != item["id"] {
		return 400, stripeErr("resource_missing")
	}
	if s["schedule"] != nil {
		return 400, stripeErr("subscription_schedule_managed")
	}
	price := form.Get("items[0][price]")
	amount := m.legacyAmountLocked(price)
	if form.Get("billing_cycle_anchor") != "now" {
		item["price"] = Object{"id": price, "unit_amount": amount, "currency": "usd"}
		return 0, nil
	}
	now := m.now().Unix()
	start, end := item["current_period_start"].(int64), item["current_period_end"].(int64)
	var credit int64
	if end > start && now < end {
		credit = item["price"].(Object)["unit_amount"].(int64) * (end - now) / (end - start)
	}
	due := max(0, amount-credit)
	pm, _ := s["default_payment_method"].(string)
	if decline := m.declines[pm]; decline != "" && due > 0 && form.Get("payment_behavior") == "error_if_incomplete" {
		return 402, Object{"error": Object{"type": "card_error", "code": "card_declined", "decline_code": decline, "message": "Your card was declined."}}
	}
	item["price"] = Object{"id": price, "unit_amount": amount, "currency": "usd"}
	item["current_period_start"], item["current_period_end"] = now, now+monthHours*3600
	m.invoiceLocked(s["id"].(string), due, true)["billing_reason"] = "subscription_update"
	return 0, nil
}

// createScheduleLocked attaches a schedule to a subscription, its one phase
// copied from the subscription's current period.
func (m *Mock) createScheduleLocked(form url.Values) (int, any) {
	sub := form.Get("from_subscription")
	s, ok := m.subs[sub]
	if !ok {
		return 400, stripeErr("resource_missing")
	}
	if s["schedule"] != nil {
		return 400, stripeErr("resource_already_exists")
	}
	item := s["items"].(Object)["data"].([]Object)[0]
	id := m.id("sub_sched")
	schedule := Object{"object": "subscription_schedule", "id": id, "subscription": sub, "status": "active", "end_behavior": "release", "metadata": map[string]string{}, "livemode": false,
		"phases": []Object{{"start_date": item["current_period_start"], "end_date": item["current_period_end"], "items": []Object{{"price": item["price"].(Object)["id"], "quantity": int64(1)}}}}}
	m.schedules[id] = schedule
	s["schedule"] = id
	return 200, schedule
}

// scheduleLocked reads a schedule or replaces its phases, end behavior and
// metadata. A phase without an end date lasts its duration.
func (m *Mock) scheduleLocked(method, id string, form url.Values) (int, any) {
	schedule, ok := m.schedules[id]
	if !ok {
		return 404, stripeErr("resource_missing")
	}
	if method == http.MethodGet {
		return 200, schedule
	}
	if method != http.MethodPost {
		return 405, stripeErr("method_not_allowed")
	}
	if v := form.Get("end_behavior"); v != "" {
		schedule["end_behavior"] = v
	}
	merged := schedule["metadata"].(map[string]string)
	for k, v := range metadataOf(form) {
		merged[k] = v
	}
	var phases []Object
	var previous int64
	for i := 0; form.Has(fmt.Sprintf("phases[%d][items][0][price]", i)); i++ {
		field := func(name string) string { return form.Get(fmt.Sprintf("phases[%d]%s", i, name)) }
		start, _ := strconv.ParseInt(field("[start_date]"), 10, 64)
		if start == 0 {
			start = previous
		}
		end, _ := strconv.ParseInt(field("[end_date]"), 10, 64)
		if end == 0 {
			count, _ := strconv.Atoi(field("[duration][interval_count]"))
			at := time.Unix(start, 0).UTC()
			switch field("[duration][interval]") {
			case "day":
				at = at.AddDate(0, 0, count)
			case "week":
				at = at.AddDate(0, 0, 7*count)
			case "year":
				at = at.AddDate(count, 0, 0)
			default:
				at = at.AddDate(0, count, 0)
			}
			end = at.Unix()
		}
		quantity, _ := strconv.ParseInt(field("[items][0][quantity]"), 10, 64)
		if quantity <= 0 {
			quantity = 1
		}
		phases = append(phases, Object{"start_date": start, "end_date": end, "items": []Object{{"price": field("[items][0][price]"), "quantity": quantity}}})
		previous = end
	}
	if len(phases) > 0 {
		schedule["phases"] = phases
	}
	return 200, schedule
}

// schedulePriceLocked is the price a subscription's schedule bills from
// start, if a phase starts there.
func (m *Mock) schedulePriceLocked(s Object, start int64) (string, bool) {
	id, _ := s["schedule"].(string)
	schedule, ok := m.schedules[id]
	if !ok {
		return "", false
	}
	for _, phase := range schedule["phases"].([]Object) {
		if phase["start_date"] == start {
			return phase["items"].([]Object)[0]["price"].(string), true
		}
	}
	return "", false
}
