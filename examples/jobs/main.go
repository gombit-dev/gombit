// Command jobs shows background jobs end to end: a typed job registered on
// the app's dispatcher, dispatched to a queue (the memory driver here; set
// GOMBIT_JOBS_DRIVER=redis for the durable one), and run by the worker.
// In an app, framework.Run starts the same worker as `./server worker`;
// this example runs it in-process with framework.RunWorker.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/framework"
	"github.com/gombit-dev/gombit/jobs"
)

// SendWelcomeEmail is version 2: version 1 carried the address itself, which
// a queue should not hold. The upgrade step keeps old queued jobs runnable.
type SendWelcomeEmail struct {
	UserID uint `json:"user_id"`
}

func (SendWelcomeEmail) JobName() string { return "send_welcome_email" }
func (SendWelcomeEmail) JobVersion() int { return 2 }

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if cfg.Jobs.Driver == config.JobsDriverSync {
		cfg.Jobs.Driver = config.JobsDriverMemory // show a real queue
	}
	app, err := framework.New(framework.WithConfig(cfg))
	if err != nil {
		log.Fatal(err)
	}
	dispatcher := app.Jobs()

	var done atomic.Int32
	jobs.MustRegister(dispatcher.Registry(), func(ctx context.Context, job SendWelcomeEmail) error {
		defer done.Add(1)
		info, _ := jobs.InfoFromContext(ctx)
		fmt.Printf("welcome user %d (job %s v%d, queued as v%d, attempt %d of %d)\n",
			job.UserID, info.Name, info.Version, info.QueuedVersion, info.Attempt, info.MaxAttempts)
		return nil
	}, jobs.WithOptions(jobs.Options{
		MaxAttempts: 8,
		Timeout:     30 * time.Second,
		Backoff:     jobs.Jittered(jobs.Exponential(5*time.Second, time.Hour)),
	}), jobs.UpgradeFrom(1, func(payload json.RawMessage) (json.RawMessage, error) {
		var v1 struct {
			UserID uint   `json:"user_id"`
			Email  string `json:"email"`
		}
		if err := json.Unmarshal(payload, &v1); err != nil {
			return nil, err
		}
		return json.Marshal(SendWelcomeEmail{UserID: v1.UserID})
	}))

	ctx := context.Background()
	if _, err := dispatcher.Dispatch(ctx, SendWelcomeEmail{UserID: 42}); err != nil {
		log.Fatal(err)
	}

	// A version-1 job still in the queue from before the payload changed.
	queue := dispatcher.Queue()
	old := jobs.Envelope{ID: "legacy", Name: "send_welcome_email", Version: 1,
		Payload: json.RawMessage(`{"user_id":7,"email":"ada@example.com"}`)}
	if err := queue.Push(ctx, dispatcher.DefaultQueue(), old, time.Time{}); err != nil {
		log.Fatal(err)
	}

	// A job for later: the queue holds it until then.
	if _, err := dispatcher.Dispatch(ctx, SendWelcomeEmail{UserID: 99}, jobs.Delay(50*time.Millisecond)); err != nil {
		log.Fatal(err)
	}

	// Run the worker until all three jobs are done.
	workerCtx, stopWorker := context.WithCancel(ctx)
	go func() {
		for done.Load() < 3 {
			time.Sleep(10 * time.Millisecond)
		}
		stopWorker()
	}()
	if err := framework.RunWorker(workerCtx, app, jobs.WorkerOptions{Concurrency: 2, PollInterval: 10 * time.Millisecond}); err != nil {
		log.Fatal(err)
	}

	// A job nothing handles fails visibly, with a classified reason.
	err = dispatcher.Registry().Run(ctx, jobs.Envelope{ID: "unknown", Name: "send_invoice", Version: 1, Payload: json.RawMessage(`{}`)})
	if jobs.Classify(err) != jobs.KindUnknownJob {
		log.Fatalf("unregistered job: got %v, want unknown_job", err)
	}
	fmt.Printf("unregistered job: %s (%v)\n", jobs.Classify(err), err)
}
