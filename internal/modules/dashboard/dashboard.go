// Package dashboard is the configurable merchant dashboard: a saved widget grid
// per merchant, each widget a metrics query validated before every save.
package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/metrics"
)

// VizTypes are the supported widget visualizations. A kpi is a tile's value,
// its change and a sparkline of the same query by time; a cohort is a heatmap
// of cohort_retention by cohort and months_since_start; a funnel is a count by
// one dimension whose values are ordered steps, each drawn with the rows that
// reached it or went further; a heading is a section title and programs each
// card PSP's dispute program standing in the range's last month, both with an
// empty query.
var VizTypes = []string{"kpi", "stat", "line", "area", "bar", "donut", "table", "cohort", "funnel", "heading", "programs"}

const (
	// Heading is the viz of a section title.
	Heading = "heading"
	// Funnel is the viz of a funnel.
	Funnel = "funnel"
	// Programs is the viz of the dispute program standings
	// (GET /v1/metrics/dispute-programs).
	Programs = "programs"
	// Cohort is the viz of a cohort heatmap.
	Cohort = "cohort"
)

// Queryless reports whether a viz draws something other than a metrics
// query; its query must be empty.
func Queryless(viz string) bool { return viz == Heading || viz == Programs }

// Grid is a widget's react-grid-layout position ({i,x,y,w,h} minus the id,
// which lives on the widget).
type Grid = billing.DashboardGrid

// Widget is one dashboard tile: a metrics query, how to draw it, and where.
type Widget = billing.DashboardWidget

// Dashboard is the GET/PUT payload.
type Dashboard = billing.Dashboard

// MaxWidgets caps a layout; a dashboard is a page, not a database. The
// health overview of a merchant with every profile is under it.
const MaxWidgets = 80

// DecodePut strictly decodes the PUT /v1/merchant/dashboard body
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
			Message: fmt.Sprintf("unknown key %q in dashboard body; valid widget keys: id, title, viz, query, grid, fixed_range — query keys follow the metrics schema", field),
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
		if Queryless(w.Viz) {
			if !emptyQuery(w.Query) {
				errs = append(errs, metrics.FieldError{Code: "widget_has_query", Param: at("query"),
					Message: fmt.Sprintf("a %s widget draws no metrics query: its query must be empty", w.Viz)})
			}
			continue
		}
		plan, verr := metrics.ValidateAt(&w.Query, now)
		if verr != nil {
			for _, fe := range verr.Errors {
				fe.Param = at("query." + fe.Param)
				errs = append(errs, fe)
			}
			continue
		}
		if w.Viz == Funnel && !funnelShaped(plan) {
			errs = append(errs, metrics.FieldError{Code: "funnel_shape", Param: at("query"),
				Message: "a funnel draws one count measure by one dimension of ordered values (checkouts by step), with no time grouping"})
		}
		if w.Viz == Cohort && !(slices.Contains(w.Query.By, "cohort") && slices.Contains(w.Query.By, "months_since_start")) {
			errs = append(errs, metrics.FieldError{Code: "invalid_widget_query", Param: at("query.by"),
				Message: "a cohort widget groups by cohort and months_since_start", Valid: []string{"cohort", "months_since_start"}})
		}
	}
	if len(errs) > 0 {
		return &metrics.ValidationError{Errors: errs}
	}
	return nil
}

// funnelShaped is a query a funnel draws: one measure that is not money, by
// one dimension with a closed vocabulary, its order the funnel's.
func funnelShaped(plan *metrics.Plan) bool {
	return !plan.HasTime && len(plan.Measures) == 1 && !plan.Measures[0].Money &&
		len(plan.Dims) == 1 && len(plan.Measures[0].Values(plan.Dims[0])) > 1
}

func emptyQuery(q billing.QueryMetricsParams) bool {
	return len(q.Measures) == 0 && len(q.By) == 0 && q.Grain == "" && q.Range == nil &&
		len(q.Filters) == 0 && len(q.Order) == 0 && q.Limit == nil && q.Compare == ""
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
