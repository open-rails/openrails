package books

import (
	"fmt"
	"strings"
)

// Operation is the kind of write that posted a transfer, part of its
// idempotency coordinate. The engine composes it and a caller supplies only
// (source, source_id), so two operations never alias on one caller key.
type Operation string

// Coord is the idempotency coordinate a transfer posts at, within (merchant,
// customer, currency). All three parts are required.
type Coord struct {
	Operation Operation
	Source    string
	SourceID  string
}

// MaxCoordBytes bounds each part of a coordinate; ledger_transfers checks it.
const MaxCoordBytes = 512

// Validate refuses a partial coordinate: a blank part, or an operation that is
// only a prefix, makes a write non-idempotent or ambiguous.
func (c Coord) Validate() error {
	if op := strings.TrimSpace(string(c.Operation)); op == "" || strings.HasSuffix(op, ":") {
		return fmt.Errorf("ledger: operation required on the idempotency coordinate")
	}
	if strings.TrimSpace(c.Source) == "" || strings.TrimSpace(c.SourceID) == "" {
		return fmt.Errorf("ledger: source and source_id required on the idempotency coordinate")
	}
	if len(c.Operation) > MaxCoordBytes || len(c.Source) > MaxCoordBytes || len(c.SourceID) > MaxCoordBytes {
		return fmt.Errorf("ledger: each part of the idempotency coordinate is at most %d bytes", MaxCoordBytes)
	}
	return nil
}

func (c Coord) String() string {
	return string(c.Operation) + "/" + c.Source + "/" + c.SourceID
}
