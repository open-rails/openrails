package archivewire

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

const testMerchant = "10000000-0000-0000-0000-000000000001"

func emptyArchive(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := NewWriter(&b, testMerchant)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Table("customers"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestCopyVerifiedAndTruncation(t *testing.T) {
	good := emptyArchive(t)
	var out bytes.Buffer
	i, err := CopyVerified(&out, bytes.NewReader(good))
	if err != nil {
		t.Fatal(err)
	}
	if i.MerchantID != testMerchant || i.Rows != 0 || len(i.Digest) != 64 || !bytes.Equal(out.Bytes(), good) {
		t.Fatal(i)
	}
	for _, n := range []int{0, 1, len(good) / 2, len(good) - 1, len(good) - 2} {
		if _, err := CopyVerified(io.Discard, bytes.NewReader(good[:n])); err == nil {
			t.Fatalf("accepted truncation %d", n)
		}
	}
	for _, bad := range [][]byte{
		append(append([]byte{}, good...), []byte("{}\n")...),
		bytes.Replace(good, []byte(`"version":2`), []byte(`"version":1`), 1),
		bytes.Replace(good, []byte(`"consistency":"repeatable_read"`), []byte(`"consistency":"read_committed"`), 1),
		bytes.Replace(good, []byte(`"rows":0`), []byte(`"rows":1`), 1),
		bytes.Replace(good, []byte(`"kind":"header"`), []byte(`"kind":"header","kind":"header"`), 1),
		bytes.Replace(good, []byte(`"table":"customers"`), []byte(`"table":"merchant_secrets"`), 1),
	} {
		if _, err := CopyVerified(io.Discard, bytes.NewReader(bad)); err == nil {
			t.Fatal("accepted invalid archive")
		}
	}
}

func TestRecordBoundAndWriterOrder(t *testing.T) {
	if _, err := CopyVerified(io.Discard, strings.NewReader(strings.Repeat("x", MaxRecordBytes)+"\n")); err == nil {
		t.Fatal("accepted oversized line")
	}
	w, err := NewWriter(io.Discard, testMerchant)
	if err != nil {
		t.Fatal(err)
	}
	if w.Table("") == nil {
		t.Fatal("accepted empty table name")
	}
	if w.Close() == nil {
		t.Fatal("accepted missing tables")
	}
}
