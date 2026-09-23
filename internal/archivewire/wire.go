// Package archivewire defines the versioned, bounded merchant billing archive wire.
// It has no engine or database dependencies. SQL scalars, including JSONB, are
// strings (or null), so an intermediate client never rounds money through float64.
package archivewire

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"regexp"
)

const (
	Version              = 2
	MaxBytes       int64 = 1 << 30
	MaxRecordBytes       = 8 << 20
)

type Info struct {
	MerchantID string `json:"merchant_id"`
	Digest     string `json:"digest"`
	Rows       int64  `json:"rows"`
}

type Header struct {
	CatalogRevision          int64  `json:"catalog_revision"`
	Kind                     string `json:"kind"`
	Version                  int    `json:"version"`
	MerchantID               string `json:"merchant_id"`
	Consistency              string `json:"consistency"`
	RequiredCutoverCondition string `json:"required_cutover_condition"`
}

type Record struct {
	Kind   string    `json:"kind"`
	Table  string    `json:"table,omitempty"`
	Values []*string `json:"values,omitempty"`
	Rows   *int64    `json:"rows,omitempty"`
	Digest string    `json:"digest,omitempty"`
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validHeader(h Header) bool {
	return h.CatalogRevision >= 0 && h.Kind == "header" && h.Version == Version && h.Consistency == "repeatable_read" && h.RequiredCutoverCondition == "source_writers_stopped" && uuidPattern.MatchString(h.MerchantID) && h.MerchantID != "00000000-0000-0000-0000-000000000000"
}

// Read verifies the complete stream, invoking callbacks while it reads. Callers
// performing writes MUST roll back when Read returns any error, including a
// missing footer or trailing bytes. Record callbacks receive table and row records;
// storage-specific table order, row widths and values belong to the caller.
// Callbacks do not receive unrecognized fields.
func Read(src io.Reader, header func(Header) error, record func(Record) error) (Info, error) {
	return read(src, nil, header, record)
}

// CopyVerified forwards an archive without buffering it in memory. An error
// means the destination may contain a prefix and must not be treated as a
// successful archive. In particular, EOF is never a substitute for the footer.
// This verifies transport integrity, not the billing schema or stored values.
func CopyVerified(dst io.Writer, src io.Reader) (Info, error) {
	return read(src, dst, nil, nil)
}

func read(src io.Reader, dst io.Writer, onHeader func(Header) error, onRecord func(Record) error) (Info, error) {
	var info Info
	limited := &io.LimitedReader{R: src, N: MaxBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), MaxRecordBytes+1)
	scanner.Split(terminatedLine)
	digest := sha256.New()
	lineNo := 0
	haveTable := false
	finished := false
	for scanner.Scan() {
		line := scanner.Bytes()
		lineNo++
		if len(line)+1 > MaxRecordBytes || limited.N <= 0 {
			return info, errors.New("archive exceeds byte limit")
		}
		if finished {
			return info, errors.New("archive contains data after footer")
		}
		if lineNo == 1 {
			var h Header
			if err := decodeCanonical(line, &h); err != nil || !validHeader(h) {
				return info, errors.New("invalid archive header or unsupported version")
			}
			info.MerchantID = h.MerchantID
			if onHeader != nil {
				if err := onHeader(h); err != nil {
					return info, err
				}
			}
		} else {
			var r Record
			if err := decodeCanonical(line, &r); err != nil {
				return info, fmt.Errorf("invalid record %d: %w", lineNo, err)
			}
			switch r.Kind {
			case "table":
				if r.Table == "" || r.Values != nil || r.Rows != nil || r.Digest != "" {
					return info, errors.New("invalid archive table fields")
				}
				haveTable = true
			case "row":
				if !haveTable || r.Table != "" || r.Rows != nil || r.Digest != "" || len(r.Values) == 0 {
					return info, errors.New("invalid archive row shape")
				}
				if r.Values[0] == nil || *r.Values[0] != info.MerchantID {
					return info, errors.New("archive row belongs to another merchant")
				}
				info.Rows++
			case "footer":
				if !haveTable || r.Table != "" || r.Values != nil || r.Rows == nil || *r.Rows != info.Rows || r.Digest != hex.EncodeToString(digest.Sum(nil)) {
					return info, errors.New("archive footer count or digest mismatch")
				}
				info.Digest, finished = r.Digest, true
			default:
				return info, errors.New("unknown archive record kind")
			}
			if onRecord != nil && !finished {
				if err := onRecord(r); err != nil {
					return info, err
				}
			}
		}
		if !finished {
			_, _ = digest.Write(line)
			_, _ = digest.Write([]byte{'\n'})
		}
		if dst != nil {
			if err := writeLine(dst, line); err != nil {
				return info, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return info, fmt.Errorf("read archive: %w", err)
	}
	if limited.N <= 0 {
		return info, errors.New("archive exceeds byte limit")
	}
	if !finished {
		return info, errors.New("archive truncated: missing footer")
	}
	return info, nil
}

func terminatedLine(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return 0, nil, errors.New("archive record is missing its final newline")
	}
	return 0, nil, nil
}

// Canonical JSON rejects duplicate fields, alternative numeric encodings,
// missing fields, whitespace and unknown fields by round-tripping the typed
// record. JSON embedded inside scalar strings remains exact text.
func decodeCanonical(raw []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, canonical) {
		return errors.New("record is not canonical JSON")
	}
	return nil
}

func writeLine(w io.Writer, b []byte) error {
	n, err := w.Write(append(b, '\n'))
	if err == nil && n != len(b)+1 {
		return io.ErrShortWrite
	}
	return err
}

type Writer struct {
	w          io.Writer
	digest     hash.Hash
	bytes      int64
	rows       int64
	haveTable  bool
	closed     bool
	merchantID string
}

func NewWriter(w io.Writer, merchantID string, catalogRevision ...int64) (*Writer, error) {
	h := Header{Kind: "header", Version: Version, MerchantID: merchantID, Consistency: "repeatable_read", RequiredCutoverCondition: "source_writers_stopped"}
	if len(catalogRevision) > 0 {
		h.CatalogRevision = catalogRevision[0]
	}
	if !validHeader(h) {
		return nil, errors.New("invalid merchant UUID")
	}
	a := &Writer{w: w, digest: sha256.New(), merchantID: merchantID}
	return a, a.write(h, true)
}

func (w *Writer) Table(name string) error {
	if w.closed || name == "" {
		return errors.New("invalid table record")
	}
	w.haveTable = true
	return w.write(Record{Kind: "table", Table: name}, true)
}

func (w *Writer) Row(values []*string) error {
	if w.closed || !w.haveTable || len(values) == 0 {
		return errors.New("invalid row shape")
	}
	if values[0] == nil || *values[0] != w.merchantID {
		return errors.New("archive row belongs to another merchant")
	}
	if err := w.write(Record{Kind: "row", Values: values}, true); err != nil {
		return err
	}
	w.rows++
	return nil
}

func (w *Writer) Close() error {
	if w.closed || !w.haveTable {
		return errors.New("archive tables incomplete")
	}
	w.closed = true
	return w.write(Record{Kind: "footer", Rows: &w.rows, Digest: hex.EncodeToString(w.digest.Sum(nil))}, false)
}

func (w *Writer) write(v any, hashed bool) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b)+1 > MaxRecordBytes || w.bytes+int64(len(b)+1) > MaxBytes {
		return errors.New("archive exceeds byte limit")
	}
	if err := writeLine(w.w, b); err != nil {
		return err
	}
	w.bytes += int64(len(b) + 1)
	if hashed {
		_, _ = w.digest.Write(b)
		_, _ = w.digest.Write([]byte{'\n'})
	}
	return nil
}
