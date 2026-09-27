package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/jobs"
)

func newJobsCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "jobs",
		Short: "Inspect and manage failed background jobs",
		Long: `Manage the jobs a worker gave up on: a permanent failure, the last allowed
attempt, or an envelope that no longer decodes. The worker keeps them on
their queue with the original payload and the reason, instead of deleting
them.

  gombit jobs failed               list failed jobs, most recent first
  gombit jobs inspect <id>         show one, payload included
  gombit jobs retry <id>... | --all   put them back with fresh attempts
  gombit jobs forget <id>...       delete them
  gombit jobs purge --force [--older-than 720h]

These read the queue directly (GOMBIT_JOBS_DRIVER=redis and GOMBIT_REDIS_*),
so they run from any machine with the app's configuration; they do not need
the app's code. --queue selects the queue (default GOMBIT_JOBS_QUEUE).

A failed job's payload stays in Redis until it is retried, forgotten, or
purged, and 'inspect' prints it. Payloads should carry IDs, not secrets; see
docs/jobs.md.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	})
	cmd.PersistentFlags().String("queue", "", "queue to act on (default GOMBIT_JOBS_QUEUE)")
	cmd.AddCommand(newJobsFailedCommand(stdout), newJobsInspectCommand(stdout), newJobsRetryCommand(stdout),
		newJobsForgetCommand(stdout), newJobsPurgeCommand(stdout))
	return cmd
}

// openJobsQueue opens the configured queue for the jobs commands; tests
// replace it.
var openJobsQueue = func(cfg config.Config) (jobs.Queue, func() error, error) {
	switch cfg.Jobs.Driver {
	case config.JobsDriverRedis:
	case config.JobsDriverSync:
		return nil, nil, errors.New("GOMBIT_JOBS_DRIVER is sync: jobs run when dispatched and none are kept; failed jobs live in a redis queue")
	default:
		return nil, nil, fmt.Errorf("GOMBIT_JOBS_DRIVER is %s: its queue lives inside the app's process, out of reach of this command; failed jobs are kept by the redis driver", cfg.Jobs.Driver)
	}
	d, err := jobs.Open(cfg.Jobs, cfg.Cache.Redis, jobs.NewRegistry())
	if err != nil {
		return nil, nil, err
	}
	return d.Queue(), d.Close, nil
}

// withJobsQueue opens the queue, resolves --queue, and runs fn.
func withJobsQueue(cmd *cobra.Command, name string, fn func(ctx context.Context, q jobs.Queue, queue string) error) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	queue, err := cmd.Flags().GetString("queue")
	if err != nil {
		return err
	}
	if queue == "" {
		queue = cfg.Jobs.Queue
	}
	if !jobs.ValidName(queue) {
		return fmt.Errorf("gombit jobs %s: invalid queue name %q", name, queue)
	}
	q, closeQueue, err := openJobsQueue(cfg)
	if err != nil {
		return fmt.Errorf("gombit jobs %s: %w", name, err)
	}
	defer func() { _ = closeQueue() }()
	ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
	defer cancel()
	if err := fn(ctx, q, queue); err != nil {
		return fmt.Errorf("gombit jobs %s: %w", name, err)
	}
	return nil
}

func newJobsFailedCommand(stdout io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "failed",
		Short: "List failed jobs, most recent first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			limit, _ := cmd.Flags().GetInt("limit")
			asJSON, _ := cmd.Flags().GetBool("json")
			return withJobsQueue(cmd, "failed", func(ctx context.Context, q jobs.Queue, queue string) error {
				failed, err := q.Failed(ctx, queue, limit)
				if err != nil {
					return err
				}
				if asJSON {
					return writeJSON(stdout, failed)
				}
				if len(failed) == 0 {
					_, err := fmt.Fprintf(stdout, "No failed jobs on %s.\n", queue)
					return err
				}
				tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
				_, _ = fmt.Fprintln(tw, "ID\tJOB\tATTEMPTS\tREASON\tKIND\tFAILED AT\tERROR")
				for _, f := range failed {
					name := f.Envelope.Name
					if name == "" {
						name = "(undecodable)"
					}
					_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n", f.Envelope.ID, name, f.Attempts, f.Failure.Reason,
						f.Failure.Kind, f.Failure.At.UTC().Format(time.RFC3339), truncate(f.Failure.Error, 60))
				}
				return tw.Flush()
			})
		},
	})
	cmd.Flags().Int("limit", 50, "most jobs to list (0 lists all, read in pages; a job failing meanwhile may be missed)")
	cmd.Flags().Bool("json", false, "print the failed jobs as JSON, payloads included")
	return cmd
}

func newJobsInspectCommand(stdout io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "inspect <id>",
		Short: "Show a failed job, payload included",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			asJSON, _ := cmd.Flags().GetBool("json")
			return withJobsQueue(cmd, "inspect", func(ctx context.Context, q jobs.Queue, queue string) error {
				f, err := q.FailedJob(ctx, queue, args[0])
				if err != nil {
					return err
				}
				if asJSON {
					return writeJSON(stdout, f)
				}
				return writeFailedJob(stdout, f)
			})
		},
	})
	cmd.Flags().Bool("json", false, "print the failed job as JSON")
	return cmd
}

func newJobsRetryCommand(stdout io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "retry <id>... | --all",
		Short: "Put failed jobs back on their queue with fresh attempts",
		RunE: func(cmd *cobra.Command, args []string) error {
			all, _ := cmd.Flags().GetBool("all")
			if all == (len(args) > 0) {
				return errors.New("gombit jobs retry: give job IDs or --all")
			}
			return withJobsQueue(cmd, "retry", func(ctx context.Context, q jobs.Queue, queue string) error {
				retry := func(id string) error {
					err := q.RetryFailed(ctx, queue, id)
					if all && errors.Is(err, jobs.ErrNotFailed) {
						return nil // retried or forgotten since it was listed
					}
					if err != nil {
						return err
					}
					_, err = fmt.Fprintf(stdout, "Retrying %s on %s.\n", id, queue)
					return err
				}
				if !all {
					for _, id := range args {
						if err := retry(id); err != nil {
							return err
						}
					}
					return nil
				}
				// Page through the failed set: a retried job leaves it, so each
				// read is the next page, and an outage's worth of failures is
				// never one read. Only an empty read is the end: a job that
				// failed while a page was read or retried can be missing from
				// a short one, and the next read from the top finds it.
				total := 0
				for {
					page, err := q.Failed(ctx, queue, retryAllPage)
					if err != nil {
						return err
					}
					for _, f := range page {
						if err := retry(f.Envelope.ID); err != nil {
							return err
						}
					}
					if len(page) == 0 {
						break
					}
					total += len(page)
				}
				if total == 0 {
					_, _ = fmt.Fprintf(stdout, "No failed jobs on %s.\n", queue)
				}
				return nil
			})
		},
	})
	cmd.Flags().Bool("all", false, "retry every failed job on the queue")
	return cmd
}

// retryAllPage is how many failed jobs `retry --all` reads at a time.
var retryAllPage = 200

func newJobsForgetCommand(stdout io.Writer) *cobra.Command {
	return silence(&cobra.Command{
		Use:   "forget <id>...",
		Short: "Delete failed jobs",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withJobsQueue(cmd, "forget", func(ctx context.Context, q jobs.Queue, queue string) error {
				for _, id := range args {
					if err := q.ForgetFailed(ctx, queue, id); err != nil {
						return err
					}
					_, _ = fmt.Fprintf(stdout, "Forgot %s on %s.\n", id, queue)
				}
				return nil
			})
		},
	})
}

func newJobsPurgeCommand(stdout io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "purge --force [--older-than 720h]",
		Short: "Delete failed jobs, all or those older than a duration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			force, _ := cmd.Flags().GetBool("force")
			if !force {
				return errors.New("gombit jobs purge: deleting failed jobs cannot be undone; pass --force")
			}
			olderThan, _ := cmd.Flags().GetDuration("older-than")
			if olderThan < 0 {
				return errors.New("gombit jobs purge: --older-than must not be negative")
			}
			return withJobsQueue(cmd, "purge", func(ctx context.Context, q jobs.Queue, queue string) error {
				var before time.Time
				if olderThan > 0 {
					before = time.Now().Add(-olderThan)
				}
				n, err := q.PurgeFailed(ctx, queue, before)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(stdout, "Purged %d failed jobs from %s.\n", n, queue)
				return err
			})
		},
	})
	cmd.Flags().Bool("force", false, "confirm the deletion")
	cmd.Flags().Duration("older-than", 0, "only purge jobs that failed longer ago than this")
	return cmd
}

func writeFailedJob(w io.Writer, f jobs.FailedJob) error {
	var b strings.Builder
	fmt.Fprintf(&b, "ID:          %s\n", f.Envelope.ID)
	fmt.Fprintf(&b, "Queue:       %s\n", f.Queue)
	if f.Envelope.Name != "" {
		fmt.Fprintf(&b, "Job:         %s (version %d)\n", f.Envelope.Name, f.Envelope.Version)
		fmt.Fprintf(&b, "Enqueued at: %s\n", f.Envelope.EnqueuedAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "Attempts:    %d\n", f.Attempts)
	fmt.Fprintf(&b, "Failed at:   %s\n", f.Failure.At.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "Reason:      %s (%s)\n", f.Failure.Reason, f.Failure.Kind)
	fmt.Fprintf(&b, "Error:       %s\n", f.Failure.Error)
	for k, v := range f.Envelope.Metadata {
		fmt.Fprintf(&b, "Metadata:    %s=%s\n", k, v)
	}
	if len(f.RawEnvelope) > 0 {
		fmt.Fprintf(&b, "Envelope (does not decode):\n%s\n", f.RawEnvelope)
	} else {
		fmt.Fprintf(&b, "Payload:\n%s\n", indentJSON(f.Envelope.Payload))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func indentJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.MarshalIndent(v, "  ", "  ")
	if err != nil {
		return string(raw)
	}
	return "  " + string(out)
}

func writeJSON(w io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// truncate shortens s to n runes (not bytes, so a character is never cut in
// half), on one line.
func truncate(s string, n int) string {
	runes := []rune(strings.Join(strings.Fields(s), " "))
	if len(runes) <= n {
		return string(runes)
	}
	return string(runes[:n-1]) + "…"
}
