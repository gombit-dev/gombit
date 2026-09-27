package jobs

import (
	"bytes"
	"context"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

// Handler runs one job. Returning nil completes it; an error fails the
// attempt. It must be safe to run more than once for the same job ID.
type Handler[T Job] func(ctx context.Context, job T) error

// Upgrade rewrites a payload from one version to the next.
type Upgrade func(payload json.RawMessage) (json.RawMessage, error)

// Registration errors. Registration happens at startup, so each is a
// programming error to fix, not a runtime condition to handle.
var (
	ErrInvalidName     = errors.New("jobs: invalid job name")
	ErrDuplicateName   = errors.New("jobs: job name already registered")
	ErrInvalidJobType  = errors.New("jobs: invalid job type")
	ErrNameMismatch    = errors.New("jobs: job name does not match its registration")
	ErrVersionMismatch = errors.New("jobs: job version does not match its registration")
)

// Registry binds job names to handlers. It encodes jobs for a queue and runs
// the envelopes a queue hands back. It is safe for concurrent use; register
// every job at startup, before the first Encode or Run.
type Registry struct {
	mu          sync.RWMutex
	byName      map[string]*registration
	byType      map[reflect.Type]*registration
	propagators []Propagator
	defaults    Options
	now         func() time.Time
	newID       func() string
}

type registration struct {
	name     string
	version  int
	goType   reflect.Type
	upgrades map[int]Upgrade
	options  Options
	// decode returns a call that runs the handler on the decoded job.
	decode func(payload json.RawMessage) (func(ctx context.Context) error, error)
}

// RegistryOption configures NewRegistry.
type RegistryOption func(*Registry)

// WithPropagator adds a Propagator. Encode runs every propagator's Inject;
// Run runs every Extract, in the order they were added.
func WithPropagator(p Propagator) RegistryOption {
	return func(r *Registry) { r.propagators = append(r.propagators, p) }
}

// WithClock sets the clock that stamps Envelope.EnqueuedAt (tests).
func WithClock(now func() time.Time) RegistryOption {
	return func(r *Registry) { r.now = now }
}

// WithIDGenerator sets the generator of Envelope.ID (tests). The default is a
// random UUID.
func WithIDGenerator(newID func() string) RegistryOption {
	return func(r *Registry) { r.newID = newID }
}

// NewRegistry returns an empty registry.
func NewRegistry(opts ...RegistryOption) *Registry {
	r := &Registry{
		byName: map[string]*registration{},
		byType: map[reflect.Type]*registration{},
		now:    time.Now,
		newID:  func() string { return uuid.NewString() },
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// RegisterOption configures one Register call.
type RegisterOption func(*registerConfig)

type registerConfig struct {
	upgrades   map[int]Upgrade
	duplicates []int
	options    *Options
}

// UpgradeFrom registers the step that rewrites a version-`from` payload into
// version from+1. Run chains the steps to bring an old payload up to the
// job's JobVersion.
func UpgradeFrom(from int, fn Upgrade) RegisterOption {
	return func(c *registerConfig) {
		if c.upgrades == nil {
			c.upgrades = map[int]Upgrade{}
		}
		if _, dup := c.upgrades[from]; dup {
			c.duplicates = append(c.duplicates, from)
		}
		c.upgrades[from] = fn
	}
}

// Register binds handler to job type T under T's JobName. T must be a struct
// type (not a pointer) that encodes to JSON. It fails on an invalid name, a
// name registered twice (by this type or another), and an UpgradeFrom step outside
// [1, JobVersion).
func Register[T Job](r *Registry, handler Handler[T], opts ...RegisterOption) error {
	goType := reflect.TypeFor[T]()
	if goType.Kind() != reflect.Struct {
		return fmt.Errorf("%w: %s is not a struct type; register the struct, with a value-receiver JobName", ErrInvalidJobType, goType)
	}
	if handler == nil {
		return fmt.Errorf("%w: %s has a nil handler", ErrInvalidJobType, goType)
	}
	var zero T
	name := zero.JobName()
	if !ValidName(name) {
		return fmt.Errorf("%w: %q (%s): use 1-%d characters of a-z, 0-9, _ . : -, starting with a letter or digit", ErrInvalidName, name, goType, maxNameLen)
	}
	// The payload is the struct's fields, both ways. A custom JSON or text
	// codec on the job type sits in different method sets for Encode (the
	// value) and Run (the pointer), so the two could disagree and the
	// unknown-field rule would ack a zero job. Field types (time.Time) may
	// have their own codecs.
	for _, codec := range customCodecs {
		if goType.Implements(codec) || reflect.PointerTo(goType).Implements(codec) {
			return fmt.Errorf("%w: %s implements %s; a job's payload is its struct fields, so move the custom encoding into a field type", ErrInvalidJobType, goType, codec)
		}
	}
	if _, err := json.Marshal(zero); err != nil {
		return fmt.Errorf("%w: %s does not encode to JSON: %v", ErrInvalidJobType, goType, err)
	}
	if !goType.Implements(versionedType) && reflect.PointerTo(goType).Implements(versionedType) {
		return fmt.Errorf("%w: %s declares JobVersion on the pointer; use a value receiver, like JobName, so every value reports it", ErrInvalidJobType, goType)
	}
	version := jobVersion(zero)
	if version < 1 {
		return fmt.Errorf("%w: %s reports JobVersion %d; versions start at 1", ErrInvalidJobType, goType, version)
	}
	var cfg registerConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	if len(cfg.duplicates) > 0 {
		return fmt.Errorf("%w: %s: UpgradeFrom(%d) is given more than once", ErrInvalidJobType, goType, cfg.duplicates[0])
	}
	var options Options
	if cfg.options != nil {
		if err := cfg.options.validate(); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInvalidJobType, goType, err)
		}
		options = *cfg.options
	}
	for from, fn := range cfg.upgrades {
		if from < 1 || from >= version || fn == nil {
			return fmt.Errorf("%w: %s: UpgradeFrom(%d) is outside versions 1..%d or has no function", ErrInvalidJobType, goType, from, version-1)
		}
	}

	resolved := options.resolve(r.defaults)
	if err := resolved.validate(); err != nil {
		return fmt.Errorf("%w: %s: with the registry defaults (WithDefaultOptions), %v", ErrInvalidJobType, goType, err)
	}
	reg := &registration{
		name:     name,
		version:  version,
		goType:   goType,
		upgrades: cfg.upgrades,
		options:  resolved,
		decode: func(payload json.RawMessage) (func(ctx context.Context) error, error) {
			var job T
			if err := json.Unmarshal(payload, &job); err != nil {
				return nil, err
			}
			return func(ctx context.Context) error { return handler(ctx, job) }, nil
		},
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// The name comes from the type's zero value, so a type registered twice
	// collides on its name too.
	if prev, ok := r.byName[name]; ok {
		return fmt.Errorf("%w: %q is %s's name already, so %s cannot use it", ErrDuplicateName, name, prev.goType, goType)
	}
	r.byName[name] = reg
	r.byType[goType] = reg
	return nil
}

// Options returns the execution policy of the named job, defaults filled
// in. A name nothing registered gets the registry's defaults, so an unknown
// job (a rolling deploy's newer producer) is still retried and bounded.
func (r *Registry) Options(name string) Options {
	r.mu.RLock()
	reg, ok := r.byName[name]
	r.mu.RUnlock()
	if ok {
		return reg.options
	}
	return Options{}.resolve(r.defaults)
}

// MustRegister is Register that panics on error, for registration at
// startup.
func MustRegister[T Job](r *Registry, handler Handler[T], opts ...RegisterOption) {
	if err := Register(r, handler, opts...); err != nil {
		panic(err)
	}
}

// Has reports whether a handler is registered for name.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.byName[name]
	return ok
}

// Names returns the registered job names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Encode turns a registered job into an envelope for a queue: a fresh ID,
// the registered name and version, the JSON payload, and the metadata the
// propagators take from ctx. An unregistered job fails here, at dispatch,
// rather than later in a worker.
func (r *Registry) Encode(ctx context.Context, job Job) (Envelope, error) {
	if job == nil {
		return Envelope{}, fmt.Errorf("%w: nil job", ErrInvalidJobType)
	}
	goType := reflect.TypeOf(job)
	value := reflect.ValueOf(job)
	if goType.Kind() == reflect.Pointer {
		if value.IsNil() {
			return Envelope{}, fmt.Errorf("%w: nil %s", ErrInvalidJobType, goType)
		}
		// *T and T are one registration, so they must be one encoding: the
		// struct value's, the method set Register checked.
		goType = goType.Elem()
		value = value.Elem()
		if j, ok := value.Interface().(Job); ok {
			job = j
		}
	}
	name := job.JobName()

	r.mu.RLock()
	reg, ok := r.byType[goType]
	r.mu.RUnlock()
	if !ok {
		return Envelope{}, &Error{Kind: KindUnknownJob, Name: name, Err: fmt.Errorf("%s is not registered", goType)}
	}
	if name != reg.name {
		return Envelope{}, fmt.Errorf("%w: %s was registered as %q but this value names itself %q; JobName must be a constant", ErrNameMismatch, goType, reg.name, name)
	}
	if v := jobVersion(job); v != reg.version {
		return Envelope{}, fmt.Errorf("%w: %s was registered at version %d but this value reports %d; JobVersion must be a constant", ErrVersionMismatch, goType, reg.version, v)
	}
	payload, err := json.Marshal(value.Interface())
	if err != nil {
		return Envelope{}, fmt.Errorf("jobs: encode %q payload: %w", name, err)
	}
	env := Envelope{
		ID:         r.newID(),
		Name:       reg.name,
		Version:    reg.version,
		Payload:    payload,
		EnqueuedAt: r.now().UTC(),
	}
	if len(r.propagators) > 0 {
		metadata := map[string]string{}
		for _, p := range r.propagators {
			p.Inject(ctx, metadata)
		}
		if len(metadata) > 0 {
			env.Metadata = metadata
		}
	}
	return env, nil
}

// Run decodes env and calls its handler with a context carrying the job's
// Info and the propagated metadata. Every failure is an *Error; Classify
// reports its Kind. A panicking handler is recovered as KindPanic, so one job
// cannot take a worker down.
func (r *Registry) Run(ctx context.Context, env Envelope) (err error) {
	if err := env.validate(); err != nil {
		return err
	}
	r.mu.RLock()
	reg, ok := r.byName[env.Name]
	r.mu.RUnlock()
	if !ok {
		return &Error{Kind: KindUnknownJob, Name: env.Name, Version: env.Version}
	}
	// The run's span, started once the propagators restored the trace. Its
	// end is deferred before the recovery below so it sees a recovered panic.
	var span trace.Span
	defer func() {
		if span != nil {
			endRunSpan(span, err)
		}
	}()
	// Everything past the lookup runs application code (upgrade steps, a
	// payload's UnmarshalJSON, propagators, the handler), so a panic anywhere
	// in it is this job's failure, not the worker's.
	defer func() {
		if p := recover(); p != nil {
			err = &Error{Kind: KindPanic, Name: reg.name, Version: reg.version, Err: fmt.Errorf("%v", p)}
		}
	}()
	// Restore the dispatching context and start the run's span first, so a
	// run that fails in an upgrade step or the decoder is traced too.
	for _, p := range r.propagators {
		ctx = p.Extract(ctx, env.Metadata)
	}
	ctx, span = startRunSpan(ctx, reg.name, env.ID, reg.version, env.Attempt)
	payload, err := reg.upgrade(env)
	if err != nil {
		return err
	}
	call, err := reg.decode(payload)
	if err != nil {
		return &Error{Kind: KindDecode, Name: reg.name, Version: reg.version, Err: err}
	}
	ctx = context.WithValue(ctx, infoKey{}, Info{
		ID:            env.ID,
		Name:          reg.name,
		Version:       reg.version,
		QueuedVersion: env.Version,
		Attempt:       env.Attempt,
		MaxAttempts:   reg.options.MaxAttempts,
		EnqueuedAt:    env.EnqueuedAt,
	})
	if reg.options.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, reg.options.Timeout, errJobTimeout)
		defer cancel()
	}
	if err := call(ctx); err != nil {
		// Only the job's own deadline is a timeout; a caller's earlier one (a
		// request deadline around a sync dispatch) is the caller's.
		if errors.Is(context.Cause(ctx), errJobTimeout) {
			return &Error{Kind: KindTimeout, Name: reg.name, Version: reg.version,
				Err: fmt.Errorf("after %s: %w", reg.options.Timeout, err)}
		}
		return &Error{Kind: KindHandler, Name: reg.name, Version: reg.version, Err: err}
	}
	return nil
}

// upgrade brings env's payload to the registered version.
func (reg *registration) upgrade(env Envelope) (json.RawMessage, error) {
	// The envelope's version selects the upgrade chain. Encode always
	// writes one (>= 1); 0 is an unset field, not an older format.
	version := env.Version
	if version < 1 {
		return nil, &Error{Kind: KindDecode, Name: reg.name, Version: env.Version, Err: fmt.Errorf("invalid payload version %d", env.Version)}
	}
	if version > reg.version {
		return nil, &Error{Kind: KindUnsupportedVersion, Name: reg.name, Version: version,
			Err: fmt.Errorf("this binary handles up to version %d; a newer producer queued it", reg.version)}
	}
	payload := env.Payload
	if emptyPayload(payload) {
		return nil, &Error{Kind: KindDecode, Name: reg.name, Version: version, Err: errors.New("empty or null payload")}
	}
	for v := version; v < reg.version; v++ {
		step, ok := reg.upgrades[v]
		if !ok {
			return nil, &Error{Kind: KindUnsupportedVersion, Name: reg.name, Version: version,
				Err: fmt.Errorf("no UpgradeFrom(%d) step to reach version %d", v, reg.version)}
		}
		next, err := step(payload)
		if err != nil {
			// The step's own failure, not bad bytes: retryable, like a panic
			// in the step or a missing one (a deploy can fix it).
			return nil, &Error{Kind: KindUpgrade, Name: reg.name, Version: v,
				Err: err}
		}
		// Every step's output is checked like the queued payload: a later
		// step or the decoder would turn null into a zero-value job.
		if emptyPayload(next) {
			return nil, &Error{Kind: KindDecode, Name: reg.name, Version: version,
				Err: fmt.Errorf("upgrade from version %d returned an empty or null payload", v)}
		}
		payload = next
	}
	return payload, nil
}

// errJobTimeout is the cause of a context canceled by a job's own Timeout.
var errJobTimeout = errors.New("job timeout")

// emptyPayload reports a payload json.Unmarshal would accept as the zero
// job: nothing, or null. A job is a struct, so neither is a job.
func emptyPayload(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

var versionedType = reflect.TypeFor[Versioned]()

// customCodecs are the interfaces that would make encoding/json bypass a
// job's struct fields.
var customCodecs = []reflect.Type{
	reflect.TypeFor[json.Marshaler](),
	reflect.TypeFor[json.Unmarshaler](),
	reflect.TypeFor[encoding.TextMarshaler](),
	reflect.TypeFor[encoding.TextUnmarshaler](),
}

func jobVersion(job Job) int {
	if v, ok := job.(Versioned); ok {
		return v.JobVersion()
	}
	return 1
}
