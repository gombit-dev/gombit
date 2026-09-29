package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/jobs"
)

func TestDispatchAtDoesNotMutateCallerOptions(t *testing.T) {
	reg := newRegistry()
	jobs.MustRegister(reg, func(context.Context, sendWelcome) error { return nil })
	q := jobs.NewMemoryQueue()
	d := jobs.NewDispatcher(reg, q)
	ctx := context.Background()

	opts := make([]jobs.DispatchOption, 0, 4)
	opts = append(opts, jobs.OnQueue("mail"))
	unique := append(opts, jobs.Unique("welcome:1", time.Hour))

	if _, err := d.DispatchAt(ctx, sendWelcome{UserID: 1}, time.Now().Add(time.Hour), opts...); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dispatch(ctx, sendWelcome{UserID: 2}, unique...); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dispatch(ctx, sendWelcome{UserID: 3}, unique...); !errors.Is(err, jobs.ErrDuplicateDispatch) {
		t.Fatalf("second Dispatch with caller-owned Unique option = %v, want ErrDuplicateDispatch", err)
	}
}
