package app

import (
	"context"
	"reflect"
	"testing"
	"time"
	"unsafe"

	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

func TestProviderRefreshPeriodicJobRunsOnStartup(t *testing.T) {
	jobs, err := (&Runtime{}).buildRiverPeriodicJobs(context.Background())
	require.NoError(t, err)

	for _, job := range jobs {
		args, opts := periodicJobConstructor(t, job)()
		if _, ok := args.(riverjobs.ProviderRefreshArgs); !ok {
			continue
		}
		require.True(t, periodicJobRunOnStart(t, job))
		require.NotNil(t, opts)
		require.Equal(t, riverjobs.QueueBilling, opts.Queue)
		require.Equal(t, 4*time.Hour, opts.UniqueOpts.ByPeriod)
		return
	}
	t.Fatal("provider refresh periodic job not registered")
}

func periodicJobConstructor(t *testing.T, job *river.PeriodicJob) river.PeriodicJobConstructor {
	t.Helper()
	v := reflect.ValueOf(job).Elem().FieldByName("constructorFunc")
	return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Interface().(river.PeriodicJobConstructor)
}

func periodicJobRunOnStart(t *testing.T, job *river.PeriodicJob) bool {
	t.Helper()
	v := reflect.ValueOf(job).Elem().FieldByName("opts")
	opts := reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Interface().(*river.PeriodicJobOpts)
	return opts != nil && opts.RunOnStart
}
