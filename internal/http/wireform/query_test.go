package wireform

import (
	"errors"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

type Page struct {
	Limit  int    `form:"limit"`
	Cursor string `form:"cursor"`
}

type status string

type sample struct {
	Page
	IDs      []uuid.UUID `form:"ids,comma"`
	Keys     []string    `form:"key"`
	Customer uuid.UUID   `form:"customer_id"`
	Status   status      `form:"status"`
	Since    time.Time   `form:"since"`
	Archived *bool       `form:"archived"`
	Live     bool        `form:"live"`
	Skipped  string      `form:"-"`
}

func TestQueryRoundTrip(t *testing.T) {
	no := false
	id := uuid.MustParse("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b")
	in := sample{
		Page:     Page{Limit: 10, Cursor: "c"},
		IDs:      []uuid.UUID{id, id},
		Keys:     []string{"a,b", "c"},
		Customer: id,
		Status:   "open",
		Since:    time.Date(2026, 1, 2, 3, 4, 5, 6, time.FixedZone("x", 3600)),
		Archived: &no,
		Skipped:  "never",
	}
	q, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{
		"limit": {"10"}, "cursor": {"c"}, "ids": {id.String() + "," + id.String()}, "key": {"a,b", "c"},
		"customer_id": {id.String()}, "status": {"open"}, "since": {"2026-01-02T02:04:05.000000006Z"}, "archived": {"false"},
	}
	if !reflect.DeepEqual(q, want) {
		t.Fatalf("Encode = %v, want %v", q, want)
	}
	var out sample
	if err := Decode(q, &out); err != nil {
		t.Fatal(err)
	}
	in.Skipped, in.Since = "", in.Since.UTC()
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("Decode = %+v, want %+v", out, in)
	}
	if q, _ := Encode(sample{}); len(q) != 0 {
		t.Fatalf("zero query = %v", q)
	}
}

func TestQueryRefusals(t *testing.T) {
	var out sample
	err := Decode(url.Values{"limit": {"x"}}, &out)
	var field *FieldError
	if !errors.As(err, &field) || field.Name != "limit" {
		t.Fatalf("Decode = %v, want a limit FieldError", err)
	}
	if err := Decode(url.Values{"since": {"2026-01-02"}}, &out); err != nil || !out.Since.Equal(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("a date = %v, %v", out.Since, err)
	}
	type untagged struct{ Name string }
	if _, err := Params(reflect.TypeFor[untagged]()); err == nil {
		t.Fatal("an untagged field is accepted")
	}
}
