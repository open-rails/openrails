package app

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/internal/modules/alerting"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

type loggedWorker struct {
	riverjobs.WorkerDefaults[alerting.DeliveryArgs]
	Log *slog.Logger
}

func (loggedWorker) Work(context.Context, *river.Job[alerting.DeliveryArgs]) error { return nil }

func TestAddWorkerRefusesANilLogger(t *testing.T) {
	err := addWorker(river.NewWorkers(), &loggedWorker{Log: nil})
	if err == nil || !strings.Contains(err.Error(), "no logger at") {
		t.Fatalf("addWorker = %v, want the nil logger refused", err)
	}
	if err := addWorker(river.NewWorkers(), &loggedWorker{Log: slog.Default()}); err != nil {
		t.Fatalf("addWorker = %v", err)
	}
}
