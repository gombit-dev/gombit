package jobs

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/gombit-dev/gombit/cache"
	"github.com/gombit-dev/gombit/config"
)

// Dispatcher is what application code dispatches jobs through. It encodes a
// job with its Registry and either pushes it to a Queue or, with no queue
// (the sync driver), runs it at once.
type Dispatcher struct {
	registry     *Registry
	queue        Queue
	driver       config.JobsDriver
	defaultQueue string
	now          func() time.Time
}

// NewDispatcher returns a dispatcher over queue, or a sync dispatcher when
// queue is nil.
func NewDispatcher(registry *Registry, queue Queue, opts ...DispatcherOption) *Dispatcher {
	d := &Dispatcher{registry: registry, queue: queue, driver: config.JobsDriverSync, defaultQueue: config.DefaultJobsQueue, now: time.Now}
	if queue != nil {
		d.driver = ""
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// DispatcherOption configures NewDispatcher.
type DispatcherOption func(*Dispatcher)

// WithDefaultQueue sets the queue jobs go to unless dispatched OnQueue.
func WithDefaultQueue(name string) DispatcherOption {
	return func(d *Dispatcher) { d.defaultQueue = name }
}

// WithDispatcherClock sets the clock Delay is measured from (tests of
// delayed jobs; give the queue the same clock).
func WithDispatcherClock(now func() time.Time) DispatcherOption {
	return func(d *Dispatcher) { d.now = now }
}

// withDriver records the configured driver name (Open).
func withDriver(driver config.JobsDriver) DispatcherOption {
	return func(d *Dispatcher) { d.driver = driver }
}

// Open opens the configured driver. The redis driver connects with redis,
// the shared GOMBIT_REDIS_* settings; the others ignore it. Jobs registered
// on registry are the ones the dispatcher can encode and a worker can run.
func Open(cfg config.JobsConfig, redis config.RedisConfig, registry *Registry) (*Dispatcher, error) {
	if err := config.ValidateJobs(cfg); err != nil {
		return nil, err
	}
	var queue Queue
	switch cfg.Driver {
	case config.JobsDriverSync:
	case config.JobsDriverMemory:
		queue = NewMemoryQueue()
	case config.JobsDriverRedis:
		client := goredis.NewClient(RedisClientOptions(cache.RedisOptions(redis)))
		queue = NewRedisQueue(client, cfg.Namespace, WithRedisClientOwned())
	default:
		return nil, fmt.Errorf("jobs: unsupported driver %q", cfg.Driver)
	}
	return NewDispatcher(registry, queue, WithDefaultQueue(cfg.Queue), withDriver(cfg.Driver)), nil
}

// OpenWithRedis opens the redis driver against the server client talks to,
// with the same address, credentials, and TLS, but on a connection pool of
// its own configured for queue calls (RedisClientOptions), which the
// dispatcher closes. client itself is left alone. cfg.Driver must be redis.
func OpenWithRedis(cfg config.JobsConfig, client *goredis.Client, registry *Registry) (*Dispatcher, error) {
	if err := config.ValidateJobs(cfg); err != nil {
		return nil, err
	}
	if cfg.Driver != config.JobsDriverRedis {
		return nil, fmt.Errorf("jobs: OpenWithRedis needs the redis driver, got %q", cfg.Driver)
	}
	if client == nil {
		return nil, fmt.Errorf("jobs: OpenWithRedis: nil client")
	}
	own := goredis.NewClient(RedisClientOptions(client.Options()))
	queue := NewRedisQueue(own, cfg.Namespace, WithRedisClientOwned())
	return NewDispatcher(registry, queue, WithDefaultQueue(cfg.Queue), withDriver(cfg.Driver)), nil
}

// DispatchOption configures one Dispatch.
type DispatchOption func(*dispatchConfig)

type dispatchConfig struct {
	queue  string
	delay  time.Duration
	at     time.Time
	unique *UniqueKey
}

// OnQueue sends the job to the named queue instead of the default one.
func OnQueue(name string) DispatchOption {
	return func(c *dispatchConfig) { c.queue = name }
}

// Delay makes the job available d from now instead of at once. The queue
// holds it until then; nothing in the application polls. "Now" is the
// dispatching host's clock and the queue compares it with the worker's, so
// hosts need synchronized clocks (NTP), as they do for leases. Between Delay
// and At, the last option given wins.
func Delay(d time.Duration) DispatchOption {
	return func(c *dispatchConfig) { c.delay, c.at = d, time.Time{} }
}

// At makes the job available at t instead of at once. A time in the past
// means now.
func At(t time.Time) DispatchOption {
	return func(c *dispatchConfig) { c.at, c.delay = t, 0 }
}

// DispatchAt is Dispatch with At(when); when overrides any Delay or At in
// opts.
func (d *Dispatcher) DispatchAt(ctx context.Context, job Job, when time.Time, opts ...DispatchOption) (Envelope, error) {
	return d.Dispatch(ctx, job, append(opts, At(when))...)
}

// Dispatch encodes job and queues it. It returns the envelope it queued, whose
// ID identifies the job from here on.
//
// With the sync driver the job runs before Dispatch returns, on ctx, and
// Dispatch returns the job's failure, if any, alongside the envelope. That is
// the development convenience the sync driver exists for (it runs a Delay or
// At job at once, and a Unique job every time: there is nothing to hold it
// or to hold a key); a queued driver returns once the job is stored.
func (d *Dispatcher) Dispatch(ctx context.Context, job Job, opts ...DispatchOption) (Envelope, error) {
	cfg := dispatchConfig{queue: d.defaultQueue}
	for _, opt := range opts {
		opt(&cfg)
	}
	if !ValidName(cfg.queue) {
		return Envelope{}, fmt.Errorf("%w: %q", ErrInvalidQueue, cfg.queue)
	}
	if cfg.delay < 0 {
		return Envelope{}, fmt.Errorf("jobs: negative delay %s", cfg.delay)
	}
	if cfg.unique != nil {
		if err := cfg.unique.validate(); err != nil {
			return Envelope{}, err
		}
	}
	env, err := d.registry.Encode(ctx, job)
	if err != nil {
		return Envelope{}, err
	}
	if d.queue == nil {
		env.Attempt = 1
		return env, d.registry.Run(ctx, env)
	}
	at := cfg.at
	if cfg.delay > 0 {
		at = d.now().Add(cfg.delay)
	}
	if cfg.unique != nil {
		if err := d.queue.PushUnique(ctx, cfg.queue, env, at, *cfg.unique); err != nil {
			return Envelope{}, err
		}
		return env, nil
	}
	if err := d.queue.Push(ctx, cfg.queue, env, at); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

// Registry is the registry the dispatcher encodes with. Register jobs on it
// at startup.
func (d *Dispatcher) Registry() *Registry { return d.registry }

// Queue is the underlying queue, or nil for the sync driver.
func (d *Dispatcher) Queue() Queue { return d.queue }

// Driver is the driver name Open selected: sync for NewDispatcher without a
// queue, and empty for NewDispatcher over a queue it was handed.
func (d *Dispatcher) Driver() config.JobsDriver { return d.driver }

// DefaultQueue is the queue Dispatch uses without OnQueue.
func (d *Dispatcher) DefaultQueue() string { return d.defaultQueue }

// Close closes the queue, if any.
func (d *Dispatcher) Close() error {
	if d.queue == nil {
		return nil
	}
	return d.queue.Close()
}
