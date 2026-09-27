// Command jobs shows background jobs end to end: a typed job registered on
// the app's dispatcher, dispatched to a queue (the memory driver here; set
// GOMBIT_JOBS_DRIVER=redis for the durable one), and consumed the way
// `gombit worker` will: reserve, run, ack.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

	jobs.MustRegister(dispatcher.Registry(), func(ctx context.Context, job SendWelcomeEmail) error {
		info, _ := jobs.InfoFromContext(ctx)
		fmt.Printf("welcome user %d (job %s v%d, queued as v%d, attempt %d)\n",
			job.UserID, info.Name, info.Version, info.QueuedVersion, info.Attempt)
		return nil
	}, jobs.UpgradeFrom(1, func(payload json.RawMessage) (json.RawMessage, error) {
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

	// The worker loop, by hand.
	for {
		delivery, err := queue.Reserve(ctx, []string{dispatcher.DefaultQueue()}, time.Minute)
		if errors.Is(err, jobs.ErrNoJob) {
			break
		}
		if err != nil {
			log.Fatal(err)
		}
		if delivery.Err != nil {
			// A stored envelope that no longer decodes: it can never run, but
			// it is leased, so ack it rather than meet it again.
			log.Printf("dropping undecodable job %s: %v", delivery.Envelope.ID, delivery.Err)
			_ = queue.Ack(ctx, delivery)
			continue
		}
		if err := dispatcher.Registry().Run(ctx, delivery.Envelope); err != nil {
			log.Printf("%s failed (%s): %v", delivery.Envelope.Name, jobs.Classify(err), err)
			_ = queue.Release(ctx, delivery, time.Now().Add(time.Minute))
			continue
		}
		if err := queue.Ack(ctx, delivery); err != nil {
			log.Fatal(err)
		}
	}

	// A job nothing handles fails visibly, with a classified reason.
	err = dispatcher.Registry().Run(ctx, jobs.Envelope{ID: "unknown", Name: "send_invoice", Version: 1, Payload: json.RawMessage(`{}`)})
	if jobs.Classify(err) != jobs.KindUnknownJob {
		log.Fatalf("unregistered job: got %v, want unknown_job", err)
	}
	fmt.Printf("unregistered job: %s (%v)\n", jobs.Classify(err), err)
}
