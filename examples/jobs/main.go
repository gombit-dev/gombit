// Command jobs shows the job contract (JOBS-1): a typed job, its handler,
// and the envelope a queue driver stores between dispatch and a worker.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

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
	registry := jobs.NewRegistry(jobs.WithPropagator(framework.JobPropagator()))
	jobs.MustRegister(registry, func(ctx context.Context, job SendWelcomeEmail) error {
		info, _ := jobs.InfoFromContext(ctx)
		fmt.Printf("welcome user %d (job %s, attempt %d, request %q)\n",
			job.UserID, info.Name, info.Attempt, framework.GetRequestIDFromContext(ctx))
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

	// Dispatch: encode the job into the envelope a queue stores.
	env, err := registry.Encode(ctx, SendWelcomeEmail{UserID: 42})
	if err != nil {
		log.Fatal(err)
	}
	stored, err := env.Marshal()
	if err != nil {
		log.Fatal(err)
	}

	// A worker, possibly another process: decode and run.
	loaded, err := jobs.UnmarshalEnvelope(stored)
	if err != nil {
		log.Fatal(err)
	}
	loaded.Attempt = 1
	if err := registry.Run(ctx, loaded); err != nil {
		log.Fatal(err)
	}

	// A version-1 job still in a queue runs through the upgrade step.
	old := jobs.Envelope{ID: "legacy", Name: "send_welcome_email", Version: 1,
		Payload: json.RawMessage(`{"user_id":7,"email":"ada@example.com"}`), Attempt: 1}
	if err := registry.Run(ctx, old); err != nil {
		log.Fatal(err)
	}

	// A job nothing handles fails visibly, with a classified reason.
	err = registry.Run(ctx, jobs.Envelope{ID: "unknown", Name: "send_invoice", Version: 1, Payload: json.RawMessage(`{}`)})
	if jobs.Classify(err) != jobs.KindUnknownJob {
		log.Fatalf("unregistered job: got %v, want unknown_job", err)
	}
	fmt.Printf("unregistered job: %s (%v)\n", jobs.Classify(err), err)
}
