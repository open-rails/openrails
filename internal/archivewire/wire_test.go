package archivewire

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

const merchant = "10000000-0000-0000-0000-000000000001"

func archive(t *testing.T, rows ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := NewWriter(&b, merchant, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Table("customers"); err != nil {
		t.Fatal(err)
	}
	for _, v := range rows {
		mid, v := merchant, v
		if err := w.Row([]*string{&mid, &v, nil}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestArchiveRoundTripsExactly(t *testing.T) {
	good := archive(t, "9223372036854775807", `{"amount":"9007199254740993"}`)
	var out bytes.Buffer
	info, err := CopyVerified(&out, bytes.NewReader(good))
	if err != nil || info.MerchantID != merchant || info.Rows != 2 || len(info.Digest) != 64 || !bytes.Equal(out.Bytes(), good) {
		t.Fatalf("copy: %+v %v", info, err)
	}
	var kinds []string
	var values []*string
	var header Header
	_, err = Read(bytes.NewReader(good), func(h Header) error { header = h; return nil }, func(r Record) error {
		kinds = append(kinds, r.Kind)
		if r.Kind == "row" {
			values = r.Values
		}
		return nil
	})
	if err != nil || header.CatalogRevision != 7 || strings.Join(kinds, ",") != "table,row,row" || *values[1] != `{"amount":"9007199254740993"}` || values[2] != nil {
		t.Fatalf("read: %v %+v %v", err, header, kinds)
	}
}

func TestArchiveRefusesIncompleteOrAlteredStreams(t *testing.T) {
	good := archive(t, "x")
	for _, n := range []int{0, 1, len(good) / 2, len(good) - 1, len(good) - 2} {
		if _, err := CopyVerified(io.Discard, bytes.NewReader(good[:n])); err == nil {
			t.Errorf("accepted truncation at %d", n)
		}
	}
	for name, bad := range map[string][]byte{
		"data after footer":   append(append([]byte{}, good...), "{}\n"...),
		"old version":         bytes.Replace(good, []byte(`"version":2`), []byte(`"version":1`), 1),
		"weaker consistency":  bytes.Replace(good, []byte(`"consistency":"repeatable_read"`), []byte(`"consistency":"read_committed"`), 1),
		"row count":           bytes.Replace(good, []byte(`"rows":1`), []byte(`"rows":2`), 1),
		"duplicate field":     bytes.Replace(good, []byte(`"kind":"header"`), []byte(`"kind":"header","kind":"header"`), 1),
		"non-canonical space": bytes.Replace(good, []byte(`"kind":"table"`), []byte(`"kind": "table"`), 1),
		"table renamed":       bytes.Replace(good, []byte(`"table":"customers"`), []byte(`"table":"merchant_secrets"`), 1),
		"row value altered":   bytes.Replace(good, []byte(`"x"`), []byte(`"y"`), 1),
		"foreign row":         bytes.Replace(good, []byte(`"values":["`+merchant), []byte(`"values":["10000000-0000-0000-0000-000000000002`), 1),
		"oversized record":    []byte(strings.Repeat("x", MaxRecordBytes) + "\n"),
	} {
		if _, err := CopyVerified(io.Discard, bytes.NewReader(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	stop := io.ErrUnexpectedEOF
	if _, err := Read(bytes.NewReader(good), nil, func(Record) error { return stop }); err != stop {
		t.Fatalf("callback error must abort the read, got %v", err)
	}
}

func TestWriterEnforcesShape(t *testing.T) {
	for _, id := range []string{"", "00000000-0000-0000-0000-000000000000", strings.ToUpper("a" + merchant[1:])} {
		if _, err := NewWriter(io.Discard, id); err == nil {
			t.Errorf("merchant %q accepted", id)
		}
	}
	if _, err := NewWriter(io.Discard, merchant, -1); err == nil {
		t.Error("negative catalog revision accepted")
	}
	w, err := NewWriter(io.Discard, merchant)
	if err != nil {
		t.Fatal(err)
	}
	mid, other := merchant, "10000000-0000-0000-0000-000000000002"
	if w.Row([]*string{&mid}) == nil {
		t.Error("row before any table accepted")
	}
	if w.Close() == nil {
		t.Error("archive without tables closed")
	}
	if w.Table("") == nil {
		t.Error("empty table name accepted")
	}
	if err := w.Table("customers"); err != nil {
		t.Fatal(err)
	}
	for name, row := range map[string][]*string{"empty": {}, "null merchant": {nil}, "foreign merchant": {&other}} {
		if w.Row(row) == nil {
			t.Errorf("%s row accepted", name)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if w.Table("late") == nil || w.Row([]*string{&mid}) == nil || w.Close() == nil {
		t.Error("writer accepted records after footer")
	}
}
