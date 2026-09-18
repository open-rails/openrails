package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sharedformat "github.com/open-rails/openrails/internal/shared/format"
)

// StripeTierChangeKeyMetadata stamps the tier-change operation id on the
// Stripe objects it mutates, so a read-back is attributable to exactly one
// operation.
const StripeTierChangeKeyMetadata = "openrails_tier_change"

// ErrStripeTierChangeMismatch reports a Stripe object that exists but is not
// the operation's frozen outcome.
var ErrStripeTierChangeMismatch = errors.New("stripe object does not match the tier change operation")

// StripeSubscriptionState is one Stripe subscription read back by its exact
// id (or returned by the update that mutated it).
type StripeSubscriptionState struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	ItemID          string `json:"item_id"`
	PriceID         string `json:"price_id"`
	InternalPriceID string `json:"internal_price_id,omitempty"`
	TierChangeKey   string `json:"tier_change_key,omitempty"`
	ScheduleID      string `json:"schedule_id,omitempty"`
	LatestInvoiceID string `json:"latest_invoice_id,omitempty"`
	PeriodStart     int64  `json:"period_start,omitempty"`
	PeriodEnd       int64  `json:"period_end,omitempty"`
}

// MatchesPriceChange is the one exact-receipt check for a Stripe price
// change, shared by the submission, the verifier read-back and operator
// resolution: the object must be the frozen subscription, carry the operation
// key, and bill the frozen Stripe price mapped to the frozen local price.
func (s StripeSubscriptionState) MatchesPriceChange(subscriptionID, key, stripePriceID, internalPriceID string) error {
	switch {
	case subscriptionID == "" || s.ID != subscriptionID:
		return fmt.Errorf("%w: subscription %q is not %q", ErrStripeTierChangeMismatch, s.ID, subscriptionID)
	case key == "" || s.TierChangeKey != key:
		return fmt.Errorf("%w: subscription %s does not carry this operation's key", ErrStripeTierChangeMismatch, s.ID)
	case stripePriceID == "" || s.PriceID != stripePriceID:
		return fmt.Errorf("%w: subscription %s bills %q, not %q", ErrStripeTierChangeMismatch, s.ID, s.PriceID, stripePriceID)
	case internalPriceID == "" || s.InternalPriceID != internalPriceID:
		return fmt.Errorf("%w: subscription %s maps to local price %q, not %q", ErrStripeTierChangeMismatch, s.ID, s.InternalPriceID, internalPriceID)
	}
	return nil
}

// Period is the billing period Stripe reports for the subscription (its
// first item's current period); ok=false when the object carries none.
func (s StripeSubscriptionState) Period() (start, end time.Time, ok bool) {
	if s.PeriodStart <= 0 || s.PeriodEnd <= s.PeriodStart {
		return time.Time{}, time.Time{}, false
	}
	return time.Unix(s.PeriodStart, 0).UTC(), time.Unix(s.PeriodEnd, 0).UTC(), true
}

// StripePriceChangeParams is one frozen subscription price change. Key is
// the operation id: it is the Stripe idempotency key and the metadata stamp.
// PaymentBehavior "error_if_incomplete" makes Stripe refuse (402) an update
// whose invoice cannot be paid instead of applying it with an open invoice
// (its default, allow_incomplete).
type StripePriceChangeParams struct {
	SubscriptionID     string
	ItemID             string
	StripePriceID      string
	InternalPriceID    string
	Key                string
	ProrationBehavior  string
	BillingCycleAnchor string
	PaymentBehavior    string
}

func (p StripePriceChangeParams) values() url.Values {
	values := url.Values{}
	values.Set("items[0][id]", p.ItemID)
	values.Set("items[0][price]", p.StripePriceID)
	values.Set("metadata[internal_price_id]", p.InternalPriceID)
	values.Set("metadata["+StripeTierChangeKeyMetadata+"]", p.Key)
	if p.ProrationBehavior != "" {
		values.Set("proration_behavior", p.ProrationBehavior)
	}
	if p.BillingCycleAnchor != "" {
		values.Set("billing_cycle_anchor", p.BillingCycleAnchor)
	}
	if p.PaymentBehavior != "" {
		values.Set("payment_behavior", p.PaymentBehavior)
	}
	return values
}

// ChangeSubscriptionPrice swaps the subscription's line item to the frozen
// price under the operation's idempotency key and returns the object Stripe
// answered with. The caller matches it against the frozen facts.
func (s *StripeService) ChangeSubscriptionPrice(ctx context.Context, p StripePriceChangeParams) (StripeSubscriptionState, error) {
	if strings.TrimSpace(p.SubscriptionID) == "" || strings.TrimSpace(p.ItemID) == "" || strings.TrimSpace(p.StripePriceID) == "" || strings.TrimSpace(p.InternalPriceID) == "" || strings.TrimSpace(p.Key) == "" {
		return StripeSubscriptionState{}, errors.New("subscription, item, price, local price and operation key are required")
	}
	body, err := s.stripePostForm(ctx, "/v1/subscriptions/"+url.PathEscape(p.SubscriptionID), p.values(), p.Key+":update")
	if err != nil {
		return StripeSubscriptionState{}, err
	}
	return parseStripeSubscriptionState(body)
}

// GetSubscriptionState reads one Stripe subscription by its exact id.
// found=false on 404.
func (s *StripeService) GetSubscriptionState(ctx context.Context, subscriptionID string) (StripeSubscriptionState, bool, error) {
	subscriptionID = strings.TrimSpace(subscriptionID)
	if subscriptionID == "" {
		return StripeSubscriptionState{}, false, errors.New("stripe subscription id is required")
	}
	body, status, err := s.stripeGet(ctx, "/v1/subscriptions/"+url.PathEscape(subscriptionID), nil)
	if err != nil {
		return StripeSubscriptionState{}, false, err
	}
	if status == http.StatusNotFound {
		return StripeSubscriptionState{}, false, nil
	}
	if status >= 400 {
		return StripeSubscriptionState{}, false, parseStripeAPIError(status, body)
	}
	state, err := parseStripeSubscriptionState(body)
	if err != nil {
		return StripeSubscriptionState{}, false, err
	}
	return state, true, nil
}

func parseStripeSubscriptionState(body []byte) (StripeSubscriptionState, error) {
	var raw struct {
		ID          string            `json:"id"`
		Status      string            `json:"status"`
		Metadata    map[string]string `json:"metadata"`
		PeriodStart int64             `json:"current_period_start"`
		PeriodEnd   int64             `json:"current_period_end"`
		Schedule    json.RawMessage   `json:"schedule"`
		Invoice     json.RawMessage   `json:"latest_invoice"`
		Items       struct {
			Data []struct {
				ID    string `json:"id"`
				Price struct {
					ID string `json:"id"`
				} `json:"price"`
				PeriodStart int64 `json:"current_period_start"`
				PeriodEnd   int64 `json:"current_period_end"`
			} `json:"data"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return StripeSubscriptionState{}, fmt.Errorf("parse stripe subscription: %w", err)
	}
	if strings.TrimSpace(raw.ID) == "" {
		return StripeSubscriptionState{}, errors.New("stripe subscription response has no id")
	}
	out := StripeSubscriptionState{ID: raw.ID, Status: raw.Status,
		InternalPriceID: strings.TrimSpace(raw.Metadata["internal_price_id"]),
		TierChangeKey:   strings.TrimSpace(raw.Metadata[StripeTierChangeKeyMetadata]),
		ScheduleID:      stripeObjectID(raw.Schedule), LatestInvoiceID: stripeObjectID(raw.Invoice)}
	// Current API versions report the period on the item; older ones on the
	// subscription itself.
	out.PeriodStart, out.PeriodEnd = raw.PeriodStart, raw.PeriodEnd
	if len(raw.Items.Data) > 0 {
		item := raw.Items.Data[0]
		out.ItemID, out.PriceID = strings.TrimSpace(item.ID), strings.TrimSpace(item.Price.ID)
		if item.PeriodEnd > 0 {
			out.PeriodStart, out.PeriodEnd = item.PeriodStart, item.PeriodEnd
		}
	}
	return out, nil
}

// stripeObjectID reads an expandable reference: a bare id string or an
// expanded object with an id.
func stripeObjectID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var id string
	if json.Unmarshal(raw, &id) == nil {
		return strings.TrimSpace(id)
	}
	var obj struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &obj)
	return strings.TrimSpace(obj.ID)
}

// StripeSchedulePhase is one phase of a subscription schedule.
type StripeSchedulePhase struct {
	StartDate int64  `json:"start_date"`
	EndDate   int64  `json:"end_date"`
	PriceID   string `json:"price_id"`
	Quantity  int64  `json:"quantity"`
}

// StripeScheduleState is one Stripe subscription schedule read back by its
// exact id (or returned by the write that produced it).
type StripeScheduleState struct {
	ID             string                `json:"id"`
	SubscriptionID string                `json:"subscription_id"`
	Status         string                `json:"status"`
	EndBehavior    string                `json:"end_behavior,omitempty"`
	TierChangeKey  string                `json:"tier_change_key,omitempty"`
	Phases         []StripeSchedulePhase `json:"phases"`
}

// MatchesCreation checks a schedule adopted from the frozen subscription: it
// must govern that subscription and start on the frozen current price.
func (s StripeScheduleState) MatchesCreation(subscriptionID, currentPriceID string) error {
	switch {
	case subscriptionID == "" || s.SubscriptionID != subscriptionID:
		return fmt.Errorf("%w: schedule %s governs %q, not %q", ErrStripeTierChangeMismatch, s.ID, s.SubscriptionID, subscriptionID)
	case s.Status == "canceled" || s.Status == "released" || s.Status == "completed":
		return fmt.Errorf("%w: schedule %s is %s", ErrStripeTierChangeMismatch, s.ID, s.Status)
	case len(s.Phases) == 0 || currentPriceID == "" || s.Phases[0].PriceID != currentPriceID:
		return fmt.Errorf("%w: schedule %s does not start on price %q", ErrStripeTierChangeMismatch, s.ID, currentPriceID)
	}
	return nil
}

// MatchesPhases is the exact-receipt check for a scheduled downgrade: the
// schedule carries the operation key, governs the frozen subscription, keeps
// the current price until the frozen period end and then bills the new price.
func (s StripeScheduleState) MatchesPhases(subscriptionID, key, currentPriceID, newPriceID string, periodEnd int64) error {
	if err := s.MatchesCreation(subscriptionID, currentPriceID); err != nil {
		return err
	}
	switch {
	case key == "" || s.TierChangeKey != key:
		return fmt.Errorf("%w: schedule %s does not carry this operation's key", ErrStripeTierChangeMismatch, s.ID)
	case len(s.Phases) < 2 || newPriceID == "" || s.Phases[1].PriceID != newPriceID:
		return fmt.Errorf("%w: schedule %s does not continue on price %q", ErrStripeTierChangeMismatch, s.ID, newPriceID)
	case periodEnd <= 0 || s.Phases[0].EndDate != periodEnd:
		return fmt.Errorf("%w: schedule %s switches at %d, not %d", ErrStripeTierChangeMismatch, s.ID, s.Phases[0].EndDate, periodEnd)
	}
	return nil
}

// CreateScheduleFromSubscription attaches a schedule to the subscription
// under the operation's idempotency key. Stripe copies the current phase
// from the subscription; other parameters cannot be combined with
// from_subscription.
func (s *StripeService) CreateScheduleFromSubscription(ctx context.Context, subscriptionID, key string) (StripeScheduleState, error) {
	if strings.TrimSpace(subscriptionID) == "" || strings.TrimSpace(key) == "" {
		return StripeScheduleState{}, errors.New("subscription and operation key are required")
	}
	body, err := s.stripePostForm(ctx, "/v1/subscription_schedules", url.Values{"from_subscription": {subscriptionID}}, key+":schedule")
	if err != nil {
		return StripeScheduleState{}, err
	}
	return parseStripeScheduleState(body)
}

// StripeSchedulePhasesParams is the frozen two-phase downgrade: the current
// phase as Stripe recorded it at creation, then the new price.
type StripeSchedulePhasesParams struct {
	ScheduleID       string
	Key              string
	Current          StripeSchedulePhase
	NewPriceID       string
	BillingCycleDays *int
}

func (p StripeSchedulePhasesParams) values() url.Values {
	interval, intervalCount := "month", 1
	if p.BillingCycleDays != nil && *p.BillingCycleDays > 0 {
		interval, intervalCount = sharedformat.BillingCycleDaysToStripeRecurring(*p.BillingCycleDays)
	}
	quantity := p.Current.Quantity
	if quantity <= 0 {
		quantity = 1
	}
	values := url.Values{}
	values.Set("end_behavior", "release")
	values.Set("proration_behavior", "none")
	values.Set("metadata["+StripeTierChangeKeyMetadata+"]", p.Key)
	values.Set("phases[0][items][0][price]", p.Current.PriceID)
	values.Set("phases[0][items][0][quantity]", strconv.FormatInt(quantity, 10))
	values.Set("phases[0][start_date]", strconv.FormatInt(p.Current.StartDate, 10))
	values.Set("phases[0][end_date]", strconv.FormatInt(p.Current.EndDate, 10))
	values.Set("phases[1][items][0][price]", p.NewPriceID)
	values.Set("phases[1][items][0][quantity]", "1")
	values.Set("phases[1][duration][interval]", interval)
	values.Set("phases[1][duration][interval_count]", strconv.Itoa(intervalCount))
	return values
}

// SetSchedulePhases writes the frozen phases under the operation's
// idempotency key and returns the schedule Stripe answered with.
func (s *StripeService) SetSchedulePhases(ctx context.Context, p StripeSchedulePhasesParams) (StripeScheduleState, error) {
	if strings.TrimSpace(p.ScheduleID) == "" || strings.TrimSpace(p.Key) == "" || strings.TrimSpace(p.Current.PriceID) == "" || strings.TrimSpace(p.NewPriceID) == "" || p.Current.StartDate <= 0 || p.Current.EndDate <= p.Current.StartDate {
		return StripeScheduleState{}, errors.New("schedule, operation key, current phase and new price are required")
	}
	body, err := s.stripePostForm(ctx, "/v1/subscription_schedules/"+url.PathEscape(p.ScheduleID), p.values(), p.Key+":phases")
	if err != nil {
		return StripeScheduleState{}, err
	}
	return parseStripeScheduleState(body)
}

// GetScheduleState reads one Stripe subscription schedule by its exact id.
// found=false on 404.
func (s *StripeService) GetScheduleState(ctx context.Context, scheduleID string) (StripeScheduleState, bool, error) {
	scheduleID = strings.TrimSpace(scheduleID)
	if scheduleID == "" {
		return StripeScheduleState{}, false, errors.New("stripe schedule id is required")
	}
	body, status, err := s.stripeGet(ctx, "/v1/subscription_schedules/"+url.PathEscape(scheduleID), nil)
	if err != nil {
		return StripeScheduleState{}, false, err
	}
	if status == http.StatusNotFound {
		return StripeScheduleState{}, false, nil
	}
	if status >= 400 {
		return StripeScheduleState{}, false, parseStripeAPIError(status, body)
	}
	state, err := parseStripeScheduleState(body)
	if err != nil {
		return StripeScheduleState{}, false, err
	}
	return state, true, nil
}

func parseStripeScheduleState(body []byte) (StripeScheduleState, error) {
	var raw struct {
		ID           string            `json:"id"`
		Status       string            `json:"status"`
		EndBehavior  string            `json:"end_behavior"`
		Metadata     map[string]string `json:"metadata"`
		Subscription json.RawMessage   `json:"subscription"`
		Phases       []struct {
			StartDate int64 `json:"start_date"`
			EndDate   int64 `json:"end_date"`
			Items     []struct {
				Price    json.RawMessage `json:"price"`
				Quantity int64           `json:"quantity"`
			} `json:"items"`
		} `json:"phases"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return StripeScheduleState{}, fmt.Errorf("parse stripe subscription schedule: %w", err)
	}
	if strings.TrimSpace(raw.ID) == "" {
		return StripeScheduleState{}, errors.New("stripe subscription schedule response has no id")
	}
	out := StripeScheduleState{ID: raw.ID, Status: raw.Status, EndBehavior: raw.EndBehavior,
		SubscriptionID: stripeObjectID(raw.Subscription), TierChangeKey: strings.TrimSpace(raw.Metadata[StripeTierChangeKeyMetadata])}
	for _, phase := range raw.Phases {
		p := StripeSchedulePhase{StartDate: phase.StartDate, EndDate: phase.EndDate}
		if len(phase.Items) > 0 {
			p.PriceID, p.Quantity = stripeObjectID(phase.Items[0].Price), phase.Items[0].Quantity
		}
		out.Phases = append(out.Phases, p)
	}
	return out, nil
}
