package reconcile

import (
	"testing"
	"time"
)

func TestEngineSynchronizesDBComponentsToOwningClock(t *testing.T) {
	want := time.Date(2044, time.May, 6, 7, 8, 9, 0, time.FixedZone("test", 2*60*60))
	store := &PGStore{}
	writer := &PGLocalWriter{}
	engine := &Engine{
		Store:  store,
		Writer: writer,
		Now:    func() time.Time { return want },
	}

	engine.syncDBClocks()

	if got := store.now(); !got.Equal(want.UTC()) || got.Location() != time.UTC {
		t.Fatalf("store clock = %v, want %v in UTC", got, want.UTC())
	}
	if got := writer.now(); !got.Equal(want) {
		t.Fatalf("writer clock = %v, want %v", got, want)
	}
}
