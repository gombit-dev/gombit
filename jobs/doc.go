// Package jobs is Gombit's background-job contract: typed jobs with stable
// names, a registry that binds each name to its handler, and the envelope a
// queue driver stores and hands back.
//
// A job is a struct whose JobName method (value receiver) returns a constant:
//
//	type SendWelcomeEmail struct {
//		UserID uint `json:"user_id"`
//	}
//
//	func (SendWelcomeEmail) JobName() string { return "send_welcome_email" }
//
//	reg := jobs.NewRegistry()
//	jobs.MustRegister(reg, func(ctx context.Context, job SendWelcomeEmail) error {
//		return mailer.Welcome(ctx, job.UserID)
//	})
//
// The name, not the Go type, identifies the job on the wire, so renaming or
// moving the type does not strand queued jobs. The payload is the job's JSON
// encoding. Application code never imports a queue driver: a driver stores
// the Envelope that Registry.Encode produces and gives it back to
// Registry.Run, which decodes it and calls the handler.
//
// Delivery is at least once. A handler must be safe to run more than once for
// the same Envelope.ID.
package jobs
