// Package dashboard is the configurable merchant dashboard: a saved widget grid
// per merchant, each widget a metrics query validated before every save. The
// optional LLM (generate.go) turns a prompt into a validated widget.
package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/metrics"
)

// VizTypes are the supported widget visualizations.
var VizTypes = []string{"stat", "line", "area", "bar", "donut", "table"}

// Grid is a widget's react-grid-layout position ({i,x,y,w,h} minus the id,
// which lives on the widget).
type Grid = billing.DashboardGrid

// Widget is one dashboard tile: a metrics query, how to draw it, and where.
type Widget = billing.DashboardWidget

// Dashboard is the GET/PUT payload.
type Dashboard = billing.Dashboard

// MaxWidgets caps a layout; a dashboard is a page, not a database.
const MaxWidgets = 60

// DecodePut strictly decodes the PUT /v1/admin/dashboard body
// {"widgets":[...]}: unknown keys anywhere (incl. inside widget queries) fail
// loudly with corrective errors, like the metrics endpoint itself.
func DecodePut(r io.Reader) ([]Widget, *metrics.ValidationError) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, valErr(metrics.FieldError{Code: "invalid_body", Param: "body", Message: fmt.Sprintf("could not read request body: %v", err)})
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var in struct {
		Widgets []Widget `json:"widgets"`
	}
	if err := dec.Decode(&in); err != nil {
		return nil, valErr(decodeFieldError(err))
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, valErr(metrics.FieldError{Code: "invalid_body", Param: "body", Message: "request body must be a single JSON object"})
	}
	return in.Widgets, nil
}

func decodeFieldError(err error) metrics.FieldError {
	msg := err.Error()
	if strings.Contains(msg, "unknown field") {
		field := msg
		if i := strings.Index(msg, `"`); i >= 0 {
			field = strings.Trim(msg[i:], `"`)
		}
		return metrics.FieldError{
			Code:    "unknown_body_key",
			Param:   field,
			Message: fmt.Sprintf("unknown key %q in dashboard body; valid widget keys: id, title, viz, query, grid — query keys follow the metrics schema", field),
		}
	}
	return metrics.FieldError{Code: "invalid_body", Param: "body", Message: fmt.Sprintf("malformed JSON body: %v", err)}
}

func valErr(fes ...metrics.FieldError) *metrics.ValidationError {
	return &metrics.ValidationError{Errors: fes}
}

// ValidateWidgets checks every widget (shape + its query through the metrics
// compiler) and returns ALL errors at once, widget-indexed, so one round trip
// fixes everything.
func ValidateWidgets(widgets []Widget) *metrics.ValidationError {
	return validateWidgetsAt(widgets, time.Now().UTC())
}

func validateWidgetsAt(widgets []Widget, now time.Time) *metrics.ValidationError {
	var errs []metrics.FieldError
	if len(widgets) > MaxWidgets {
		errs = append(errs, metrics.FieldError{Code: "too_many_widgets", Param: "widgets",
			Message: fmt.Sprintf("%d widgets exceeds the maximum of %d", len(widgets), MaxWidgets)})
	}
	seenID := map[string]bool{}
	for i, w := range widgets {
		at := func(field string) string { return fmt.Sprintf("widgets[%d].%s", i, field) }
		if strings.TrimSpace(w.ID) == "" {
			errs = append(errs, metrics.FieldError{Code: "missing_widget_id", Param: at("id"), Message: "widget id is required"})
		} else if seenID[w.ID] {
			errs = append(errs, metrics.FieldError{Code: "duplicate_widget_id", Param: at("id"), Message: fmt.Sprintf("widget id %q used more than once", w.ID)})
		}
		seenID[w.ID] = true
		if strings.TrimSpace(w.Title) == "" {
			errs = append(errs, metrics.FieldError{Code: "missing_widget_title", Param: at("title"), Message: "widget title is required"})
		}
		if fe, ok := validateViz(w.Viz); !ok {
			fe.Param = at("viz")
			errs = append(errs, fe)
		}
		if w.Grid.W < 1 || w.Grid.H < 1 || w.Grid.X < 0 || w.Grid.Y < 0 {
			errs = append(errs, metrics.FieldError{Code: "invalid_grid", Param: at("grid"),
				Message: "grid needs x>=0, y>=0, w>=1, h>=1"})
		}
		if _, verr := metrics.ValidateAt(&w.Query, now); verr != nil {
			for _, fe := range verr.Errors {
				fe.Param = at("query." + fe.Param)
				errs = append(errs, fe)
			}
		}
	}
	if len(errs) > 0 {
		return &metrics.ValidationError{Errors: errs}
	}
	return nil
}

func validateViz(viz string) (metrics.FieldError, bool) {
	for _, v := range VizTypes {
		if viz == v {
			return metrics.FieldError{}, true
		}
	}
	return metrics.FieldError{Code: "invalid_viz",
		Message: fmt.Sprintf("unknown viz %q", viz), Valid: VizTypes}, false
}
