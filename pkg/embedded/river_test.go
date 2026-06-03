package embedded

import (
	"context"
	"testing"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
)

func TestNilEmbeddedMethods(t *testing.T) {
	ctx := context.Background()

	t.Run("AddWorkersTo returns error on nil embedded", func(t *testing.T) {
		var e *Embedded
		workers := river.NewWorkers()
		err := e.AddWorkersTo(ctx, workers)
		assert.ErrorIs(t, err, ErrNotInitialized)
	})

	t.Run("GetPeriodicJobs returns error on nil embedded", func(t *testing.T) {
		var e *Embedded
		jobs, err := e.GetPeriodicJobs(ctx)
		assert.ErrorIs(t, err, ErrNotInitialized)
		assert.Nil(t, jobs)
	})

	t.Run("SetRiverClient handles nil embedded gracefully", func(t *testing.T) {
		var e *Embedded
		// Should not panic
		e.SetRiverClient(nil)
	})

	t.Run("HasExternalRiverClient returns false on nil embedded", func(t *testing.T) {
		var e *Embedded
		assert.False(t, e.HasExternalRiverClient())
	})
}

func TestUninitializedEmbeddedMethods(t *testing.T) {
	ctx := context.Background()

	t.Run("AddWorkersTo returns error on uninitialized app", func(t *testing.T) {
		e := &Embedded{} // app is nil
		workers := river.NewWorkers()
		err := e.AddWorkersTo(ctx, workers)
		assert.ErrorIs(t, err, ErrNotInitialized)
	})

	t.Run("GetPeriodicJobs returns error on uninitialized app", func(t *testing.T) {
		e := &Embedded{} // app is nil
		jobs, err := e.GetPeriodicJobs(ctx)
		assert.ErrorIs(t, err, ErrNotInitialized)
		assert.Nil(t, jobs)
	})

	t.Run("SetRiverClient handles uninitialized app gracefully", func(t *testing.T) {
		e := &Embedded{} // app is nil
		// Should not panic
		e.SetRiverClient(nil)
	})

	t.Run("HasExternalRiverClient returns false on uninitialized app", func(t *testing.T) {
		e := &Embedded{} // app is nil
		assert.False(t, e.HasExternalRiverClient())
	})
}
