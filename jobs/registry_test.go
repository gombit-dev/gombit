package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/jobs"
)

type sendWelcome struct {
	UserID uint   `json:"user_id"`
	Locale string `json:"locale,omitempty"`
}

func (sendWelcome) JobName() string { return "send_welcome_email" }

type other struct{}

func (other) JobName() string { return "other" }

var fixedNow = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func newRegistry(opts ...jobs.RegistryOption) *jobs.Registry {
	n := 0
	base := []jobs.RegistryOption{
		jobs.WithClock(func() time.Time { return fixedNow }),
		jobs.WithIDGenerator(func() string { n++; return fmt.Sprintf("job-%d", n) }),
	}
	return jobs.NewRegistry(append(base, opts...)...)
}

func TestEncodeAndRun(t *testing.T) {
	reg := newRegistry()
	var got sendWelcome
	var info jobs.Info
	jobs.MustRegister(reg, func(ctx context.Context, job sendWelcome) error {
		got = job
		info, _ = jobs.InfoFromContext(ctx)
		return nil
	})

	env, err := reg.Encode(context.Background(), sendWelcome{UserID: 7, Locale: "pt"})
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if env.ID != "job-1" || env.Name != "send_welcome_email" || env.Version != 1 || !env.EnqueuedAt.Equal(fixedNow) || env.Attempt != 0 {
		t.Fatalf("envelope = %+v", env)
	}
	if string(env.Payload) != `{"user_id":7,"locale":"pt"}` {
		t.Fatalf("payload = %s", env.Payload)
	}
	if env.Metadata != nil {
		t.Fatalf("metadata without propagators = %v, want nil", env.Metadata)
	}

	env.Attempt = 2
	if err := reg.Run(context.Background(), env); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got != (sendWelcome{UserID: 7, Locale: "pt"}) {
		t.Fatalf("handler got %+v", got)
	}
	want := jobs.Info{ID: "job-1", Name: "send_welcome_email", Version: 1, QueuedVersion: 1, Attempt: 2, EnqueuedAt: fixedNow}
	if info != want {
		t.Fatalf("Info = %+v, want %+v", info, want)
	}
	if _, ok := jobs.InfoFromContext(context.Background()); ok {
		t.Fatal("InfoFromContext found a job outside a handler")
	}
}

// TestNamesAreStableAcrossRestarts: an envelope stored by one process runs in
// another, fresh one; the name, not a Go type identity or registration order,
// selects the handler.
func TestNamesAreStableAcrossRestarts(t *testing.T) {
	producer := newRegistry()
	jobs.MustRegister(producer, func(context.Context, other) error { return nil })
	jobs.MustRegister(producer, func(context.Context, sendWelcome) error { return nil })
	env, err := producer.Encode(context.Background(), &sendWelcome{UserID: 3})
	if err != nil {
		t.Fatalf("Encode(pointer) error = %v", err)
	}
	stored, err := env.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	worker := jobs.NewRegistry()
	var got uint
	jobs.MustRegister(worker, func(_ context.Context, job sendWelcome) error { got = job.UserID; return nil })
	loaded, err := jobs.UnmarshalEnvelope(stored)
	if err != nil {
		t.Fatalf("UnmarshalEnvelope() error = %v", err)
	}
	if err := worker.Run(context.Background(), loaded); err != nil || got != 3 {
		t.Fatalf("Run() in a new registry: err = %v, user = %d", err, got)
	}
	if names := producer.Names(); strings.Join(names, ",") != "other,send_welcome_email" {
		t.Fatalf("Names() = %v", names)
	}
}

func TestUnknownJobsFailVisibly(t *testing.T) {
	reg := newRegistry()
	jobs.MustRegister(reg, func(context.Context, other) error { return nil })

	_, err := reg.Encode(context.Background(), sendWelcome{UserID: 1})
	if !errors.Is(err, jobs.ErrUnknownJob) || jobs.Classify(err) != jobs.KindUnknownJob || !strings.Contains(err.Error(), "send_welcome_email") || !strings.Contains(err.Error(), "jobs_test.sendWelcome is not registered") {
		t.Fatalf("Encode(unregistered) error = %v, want an unknown_job naming it", err)
	}
	err = reg.Run(context.Background(), jobs.Envelope{ID: "x", Name: "send_welcome_email", Version: 1, Payload: []byte(`{}`)})
	if !errors.Is(err, jobs.ErrUnknownJob) || jobs.Classify(err) != jobs.KindUnknownJob || !strings.Contains(err.Error(), `"send_welcome_email"`) {
		t.Fatalf("Run(unregistered) error = %v, want an unknown_job naming it", err)
	}
}

func TestDecodeFailuresAreClassified(t *testing.T) {
	reg := newRegistry()
	called := false
	jobs.MustRegister(reg, func(context.Context, sendWelcome) error { called = true; return nil })

	for _, tc := range []struct {
		name    string
		payload string
	}{
		{"wrong type", `{"user_id":"seven"}`},
		{"not JSON", `{user_id: 7`},
		{"empty", ``},
		{"null", ` null `},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := reg.Run(context.Background(), jobs.Envelope{ID: "x", Name: "send_welcome_email", Version: 1, Payload: json.RawMessage(tc.payload)})
			if !errors.Is(err, jobs.ErrDecode) || jobs.Classify(err) != jobs.KindDecode {
				t.Fatalf("Run() error = %v, want a decode failure", err)
			}
			var jobErr *jobs.Error
			if !errors.As(err, &jobErr) || jobErr.Name != "send_welcome_email" {
				t.Fatalf("error = %#v, want a *jobs.Error naming the job", err)
			}
		})
	}
	if called {
		t.Fatal("the handler ran on an undecodable payload")
	}

	for _, data := range []string{`not json`, `{"id":"x","payload":{}}`, `{"name":"send_welcome_email","payload":{}}`} {
		if _, err := jobs.UnmarshalEnvelope([]byte(data)); !errors.Is(err, jobs.ErrDecode) {
			t.Fatalf("UnmarshalEnvelope(%s) error = %v, want a decode failure", data, err)
		}
	}
}

func TestHandlerFailuresAreClassified(t *testing.T) {
	reg := newRegistry()
	boom := errors.New("smtp: connection refused")
	var fail, panicking bool
	jobs.MustRegister(reg, func(context.Context, sendWelcome) error {
		if panicking {
			panic("nil map")
		}
		if fail {
			return boom
		}
		return nil
	})
	env, err := reg.Encode(context.Background(), sendWelcome{UserID: 1})
	if err != nil {
		t.Fatal(err)
	}

	fail = true
	err = reg.Run(context.Background(), env)
	if !errors.Is(err, boom) || !errors.Is(err, jobs.ErrHandler) || jobs.Classify(err) != jobs.KindHandler {
		t.Fatalf("handler error = %v, want KindHandler wrapping the cause", err)
	}

	panicking = true
	err = reg.Run(context.Background(), env)
	if !errors.Is(err, jobs.ErrPanic) || jobs.Classify(err) != jobs.KindPanic || !strings.Contains(err.Error(), "nil map") {
		t.Fatalf("panic = %v, want a recovered KindPanic", err)
	}

	if jobs.Classify(nil) != "" || jobs.Classify(errors.New("plain")) != jobs.KindHandler {
		t.Fatal("Classify(nil) must be empty and an unclassified error KindHandler")
	}
}

type pointerJob struct{}

func (*pointerJob) JobName() string { return "pointer_job" }

type badName struct{}

func (badName) JobName() string { return "Send Welcome" }

type noJSON struct{ C chan int }

func (noJSON) JobName() string { return "no_json" }

type badVersion struct{}

func (badVersion) JobName() string { return "bad_version" }
func (badVersion) JobVersion() int { return 0 }

type welcomeTwin struct{}

func (welcomeTwin) JobName() string { return "send_welcome_email" }

// byTenant names itself after a field: a registration bug Encode catches.
type byTenant struct{ Tenant string }

func (j byTenant) JobName() string {
	if j.Tenant == "" {
		return "by_tenant"
	}
	return "by_tenant." + j.Tenant
}

func TestRegisterRejectsProgrammingErrors(t *testing.T) {
	nop := func(context.Context, sendWelcome) error { return nil }
	reg := newRegistry()
	jobs.MustRegister(reg, nop)

	cases := []struct {
		name string
		err  error
		want error
	}{
		{"pointer job type", jobs.Register(reg, func(context.Context, *pointerJob) error { return nil }), jobs.ErrInvalidJobType},
		{"invalid name", jobs.Register(reg, func(context.Context, badName) error { return nil }), jobs.ErrInvalidName},
		{"payload without JSON", jobs.Register(reg, func(context.Context, noJSON) error { return nil }), jobs.ErrInvalidJobType},
		{"version below 1", jobs.Register(reg, func(context.Context, badVersion) error { return nil }), jobs.ErrInvalidJobType},
		{"name taken by another type", jobs.Register(reg, func(context.Context, welcomeTwin) error { return nil }), jobs.ErrDuplicateName},
		{"type registered twice", jobs.Register(reg, nop), jobs.ErrDuplicateName},
		{"nil handler", jobs.Register[other](reg, nil), jobs.ErrInvalidJobType},
		{"upgrade step outside the versions", jobs.Register(newRegistry(), nop, jobs.UpgradeFrom(1, func(p json.RawMessage) (json.RawMessage, error) { return p, nil })), jobs.ErrInvalidJobType},
	}
	for _, tc := range cases {
		if !errors.Is(tc.err, tc.want) {
			t.Errorf("%s: Register() error = %v, want %v", tc.name, tc.err, tc.want)
		}
	}

	defer func() {
		if recover() == nil {
			t.Fatal("MustRegister did not panic on a duplicate")
		}
	}()
	jobs.MustRegister(reg, nop)
}

func TestEncodeRejectsAValueDependentName(t *testing.T) {
	reg := newRegistry()
	jobs.MustRegister(reg, func(context.Context, byTenant) error { return nil })
	if _, err := reg.Encode(context.Background(), byTenant{}); err != nil {
		t.Fatalf("Encode(zero) error = %v", err)
	}
	if _, err := reg.Encode(context.Background(), byTenant{Tenant: "acme"}); !errors.Is(err, jobs.ErrNameMismatch) {
		t.Fatalf("Encode(named by a field) error = %v, want ErrNameMismatch", err)
	}
	if _, err := reg.Encode(context.Background(), nil); !errors.Is(err, jobs.ErrInvalidJobType) {
		t.Fatalf("Encode(nil) error = %v", err)
	}
	var nilJob *sendWelcome
	if _, err := reg.Encode(context.Background(), nilJob); !errors.Is(err, jobs.ErrInvalidJobType) {
		t.Fatalf("Encode(nil pointer) error = %v", err)
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"send_welcome_email":     true,
		"billing.invoice:v2":     true,
		"a-b":                    true,
		"0day":                   true,
		"":                       false,
		"Send":                   false,
		"_leading":               false,
		"with space":             false,
		"ünicode":                false,
		strings.Repeat("a", 128): true,
		strings.Repeat("a", 129): false,
	} {
		if got := jobs.ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}

// resize is at version 3: v1 had "size", v2 renamed it to "width", v3 added
// "height" (defaulting to width).
type resize struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (resize) JobName() string { return "resize_image" }
func (resize) JobVersion() int { return 3 }

func TestPayloadVersioning(t *testing.T) {
	v1to2 := func(p json.RawMessage) (json.RawMessage, error) {
		var old struct {
			Size int `json:"size"`
		}
		if err := json.Unmarshal(p, &old); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]int{"width": old.Size})
	}
	v2to3 := func(p json.RawMessage) (json.RawMessage, error) {
		var old struct {
			Width int `json:"width"`
		}
		if err := json.Unmarshal(p, &old); err != nil {
			return nil, err
		}
		return json.Marshal(resize{Width: old.Width, Height: old.Width})
	}
	reg := newRegistry()
	var got resize
	var info jobs.Info
	jobs.MustRegister(reg, func(ctx context.Context, job resize) error {
		got = job
		info, _ = jobs.InfoFromContext(ctx)
		return nil
	}, jobs.UpgradeFrom(1, v1to2), jobs.UpgradeFrom(2, v2to3))

	env, err := reg.Encode(context.Background(), resize{Width: 4, Height: 5})
	if err != nil || env.Version != 3 {
		t.Fatalf("Encode() = %+v, %v; want version 3", env, err)
	}
	for _, tc := range []struct {
		version int
		payload string
	}{
		{1, `{"size":8}`},
		{0, `{"size":8}`}, // an envelope from before versioning is version 1
		{2, `{"width":8}`},
	} {
		got = resize{}
		if err := reg.Run(context.Background(), jobs.Envelope{ID: "x", Name: "resize_image", Version: tc.version, Payload: json.RawMessage(tc.payload)}); err != nil {
			t.Fatalf("Run(v%d) error = %v", tc.version, err)
		}
		wantQueued := tc.version
		if wantQueued == 0 {
			wantQueued = 1
		}
		if got != (resize{Width: 8, Height: 8}) || info.Version != 3 || info.QueuedVersion != wantQueued {
			t.Fatalf("Run(v%d) handler got %+v (info version %d, queued %d), want the upgraded payload", tc.version, got, info.Version, info.QueuedVersion)
		}
	}

	newer := jobs.Envelope{ID: "x", Name: "resize_image", Version: 4, Payload: json.RawMessage(`{}`)}
	if err := reg.Run(context.Background(), newer); !errors.Is(err, jobs.ErrUnsupportedVersion) || jobs.Classify(err) != jobs.KindUnsupportedVersion {
		t.Fatalf("Run(newer version) error = %v, want unsupported_version", err)
	}
	negative := jobs.Envelope{ID: "x", Name: "resize_image", Version: -2, Payload: json.RawMessage(`{}`)}
	if err := reg.Run(context.Background(), negative); !errors.Is(err, jobs.ErrDecode) || !strings.Contains(err.Error(), "invalid payload version -2") {
		t.Fatalf("Run(negative version) error = %v, want a decode failure naming the version", err)
	}
	badUpgrade := jobs.Envelope{ID: "x", Name: "resize_image", Version: 1, Payload: json.RawMessage(`{"size":"big"}`)}
	if err := reg.Run(context.Background(), badUpgrade); !errors.Is(err, jobs.ErrDecode) {
		t.Fatalf("Run(v1 payload the upgrade cannot read) error = %v, want decode", err)
	}

	gap := newRegistry()
	jobs.MustRegister(gap, func(context.Context, resize) error { return nil }, jobs.UpgradeFrom(2, v2to3))
	err = gap.Run(context.Background(), jobs.Envelope{ID: "x", Name: "resize_image", Version: 1, Payload: json.RawMessage(`{"size":1}`)})
	if !errors.Is(err, jobs.ErrUnsupportedVersion) || !strings.Contains(err.Error(), "UpgradeFrom(1)") {
		t.Fatalf("Run(v1 with no step from 1) error = %v, want unsupported_version naming the step", err)
	}
}

// TestUnknownFieldsAreCompatible: an additive payload change needs no new
// version, in either direction of a rolling deploy.
func TestUnknownFieldsAreCompatible(t *testing.T) {
	reg := newRegistry()
	var got sendWelcome
	jobs.MustRegister(reg, func(_ context.Context, job sendWelcome) error { got = job; return nil })
	env := jobs.Envelope{ID: "x", Name: "send_welcome_email", Version: 1, Payload: json.RawMessage(`{"user_id":9,"campaign":"fall"}`)}
	if err := reg.Run(context.Background(), env); err != nil || got.UserID != 9 {
		t.Fatalf("Run(extra field) = %v, %+v", err, got)
	}
}

type ctxKey string

type keyPropagator struct{ key string }

func (p keyPropagator) Inject(ctx context.Context, md map[string]string) {
	if v, ok := ctx.Value(ctxKey(p.key)).(string); ok {
		md[p.key] = v
	}
}

func (p keyPropagator) Extract(ctx context.Context, md map[string]string) context.Context {
	if v, ok := md[p.key]; ok {
		return context.WithValue(ctx, ctxKey(p.key), v)
	}
	return ctx
}

func TestPropagatorsCarryContext(t *testing.T) {
	reg := newRegistry(jobs.WithPropagator(keyPropagator{"tenant"}), jobs.WithPropagator(keyPropagator{"locale"}))
	var tenant, locale any
	jobs.MustRegister(reg, func(ctx context.Context, _ sendWelcome) error {
		tenant, locale = ctx.Value(ctxKey("tenant")), ctx.Value(ctxKey("locale"))
		return nil
	})

	ctx := context.WithValue(context.WithValue(context.Background(), ctxKey("tenant"), "acme"), ctxKey("locale"), "pt")
	env, err := reg.Encode(ctx, sendWelcome{UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if env.Metadata["tenant"] != "acme" || env.Metadata["locale"] != "pt" {
		t.Fatalf("metadata = %v", env.Metadata)
	}
	if err := reg.Run(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if tenant != "acme" || locale != "pt" {
		t.Fatalf("handler context tenant = %v, locale = %v", tenant, locale)
	}

	bare, err := reg.Encode(context.Background(), sendWelcome{UserID: 1})
	if err != nil || bare.Metadata != nil {
		t.Fatalf("Encode(bare context) metadata = %v, err = %v; want none", bare.Metadata, err)
	}
	if err := reg.Run(context.Background(), bare); err != nil {
		t.Fatalf("Run(no metadata) error = %v", err)
	}
}

func TestRegistryIsSafeForConcurrentUse(t *testing.T) {
	reg := jobs.NewRegistry()
	jobs.MustRegister(reg, func(context.Context, sendWelcome) error { return nil })
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				env, err := reg.Encode(context.Background(), sendWelcome{UserID: uint(j)})
				if err != nil {
					t.Error(err)
					return
				}
				if err := reg.Run(context.Background(), env); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

type loud struct {
	Value loudField `json:"value"`
}

func (loud) JobName() string { return "loud" }

type loudField struct{}

func (*loudField) UnmarshalJSON([]byte) error { panic("custom decoder") }

// TestPanicsBeforeTheHandlerAreRecovered: upgrade steps, a payload's own
// UnmarshalJSON, and propagators are application code too.
func TestPanicsBeforeTheHandlerAreRecovered(t *testing.T) {
	reg := newRegistry(jobs.WithPropagator(panicky{}))
	jobs.MustRegister(reg, func(context.Context, resize) error { return nil },
		jobs.UpgradeFrom(1, func(json.RawMessage) (json.RawMessage, error) { panic("upgrade step") }),
		jobs.UpgradeFrom(2, func(p json.RawMessage) (json.RawMessage, error) { return p, nil }))
	jobs.MustRegister(reg, func(context.Context, loud) error { return nil })
	jobs.MustRegister(reg, func(context.Context, sendWelcome) error { return nil })

	for _, tc := range []struct {
		env  jobs.Envelope
		want string
	}{
		{jobs.Envelope{ID: "x", Name: "resize_image", Version: 1, Payload: json.RawMessage(`{}`)}, "upgrade step"},
		{jobs.Envelope{ID: "x", Name: "loud", Version: 1, Payload: json.RawMessage(`{"value":1}`)}, "custom decoder"},
		{jobs.Envelope{ID: "x", Name: "send_welcome_email", Version: 1, Payload: json.RawMessage(`{}`), Metadata: map[string]string{"boom": "1"}}, "propagator"},
	} {
		err := reg.Run(context.Background(), tc.env)
		if jobs.Classify(err) != jobs.KindPanic || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Run(%s) error = %v, want a recovered panic (%s)", tc.env.Name, err, tc.want)
		}
	}
}

type panicky struct{}

func (panicky) Inject(context.Context, map[string]string) {}

func (panicky) Extract(ctx context.Context, md map[string]string) context.Context {
	if md["boom"] != "" {
		panic("propagator")
	}
	return ctx
}

// TestNullNeverBecomesAJob: an upgrade step's output is checked like the
// queued payload, so null cannot reach the decoder or a later step that
// would turn it into a zero-value job.
func TestNullNeverBecomesAJob(t *testing.T) {
	reg := newRegistry()
	called := false
	jobs.MustRegister(reg, func(context.Context, resize) error { called = true; return nil },
		jobs.UpgradeFrom(1, func(json.RawMessage) (json.RawMessage, error) {
			var nothing *struct{ Width int }
			return json.Marshal(nothing) // null
		}),
		// The documented upgrade shape: unmarshal the old payload, marshal the new.
		jobs.UpgradeFrom(2, func(p json.RawMessage) (json.RawMessage, error) {
			var old struct {
				Width int `json:"width"`
			}
			if err := json.Unmarshal(p, &old); err != nil {
				return nil, err
			}
			return json.Marshal(resize{Width: old.Width, Height: old.Width})
		}))
	for _, version := range []int{1, 2} {
		payload := `{"size":7}`
		if version == 2 {
			payload = `null`
		}
		err := reg.Run(context.Background(), jobs.Envelope{ID: "x", Name: "resize_image", Version: version, Payload: json.RawMessage(payload)})
		if !errors.Is(err, jobs.ErrDecode) {
			t.Fatalf("Run(v%d through a null-returning step) error = %v, want decode", version, err)
		}
	}
	if called {
		t.Fatal("the handler ran on a zero-value job")
	}
}

type pointerMarshal struct {
	N int `json:"n"`
}

func (pointerMarshal) JobName() string { return "pointer_marshal" }

func (p *pointerMarshal) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf(`{"ptr":%d}`, p.N)), nil
}

// TestPointerAndValueEncodeAlike: *T and T are one registration, so they are
// one encoding, the struct value's.
func TestPointerAndValueEncodeAlike(t *testing.T) {
	reg := newRegistry()
	var got pointerMarshal
	jobs.MustRegister(reg, func(_ context.Context, job pointerMarshal) error { got = job; return nil })
	byValue, err := reg.Encode(context.Background(), pointerMarshal{N: 4})
	if err != nil {
		t.Fatal(err)
	}
	byPointer, err := reg.Encode(context.Background(), &pointerMarshal{N: 4})
	if err != nil {
		t.Fatal(err)
	}
	if string(byPointer.Payload) != string(byValue.Payload) || string(byValue.Payload) != `{"n":4}` {
		t.Fatalf("payloads: pointer %s, value %s; want both the struct value's", byPointer.Payload, byValue.Payload)
	}
	if err := reg.Run(context.Background(), byPointer); err != nil || got.N != 4 {
		t.Fatalf("Run = %v, got %+v", err, got)
	}
}

type pointerVersion struct{}

func (pointerVersion) JobName() string  { return "pointer_version" }
func (*pointerVersion) JobVersion() int { return 2 }

type varyingVersion struct {
	Legacy bool `json:"legacy"`
}

func (varyingVersion) JobName() string { return "varying_version" }
func (v varyingVersion) JobVersion() int {
	if v.Legacy {
		return 1
	}
	return 2
}

func TestJobVersionIsAConstantOfTheType(t *testing.T) {
	reg := newRegistry()
	if err := jobs.Register(reg, func(context.Context, pointerVersion) error { return nil }); !errors.Is(err, jobs.ErrInvalidJobType) {
		t.Fatalf("Register(pointer-receiver JobVersion) error = %v, want ErrInvalidJobType", err)
	}
	jobs.MustRegister(reg, func(context.Context, varyingVersion) error { return nil },
		jobs.UpgradeFrom(1, func(p json.RawMessage) (json.RawMessage, error) { return p, nil }))
	if _, err := reg.Encode(context.Background(), varyingVersion{}); err != nil {
		t.Fatalf("Encode(registered version) error = %v", err)
	}
	if _, err := reg.Encode(context.Background(), varyingVersion{Legacy: true}); !errors.Is(err, jobs.ErrVersionMismatch) {
		t.Fatalf("Encode(value reporting another version) error = %v, want ErrVersionMismatch", err)
	}
}

func TestRepeatedUpgradeStepIsRejected(t *testing.T) {
	step := func(p json.RawMessage) (json.RawMessage, error) { return p, nil }
	err := jobs.Register(newRegistry(), func(context.Context, resize) error { return nil },
		jobs.UpgradeFrom(1, step), jobs.UpgradeFrom(1, step), jobs.UpgradeFrom(2, step))
	if !errors.Is(err, jobs.ErrInvalidJobType) || !strings.Contains(err.Error(), "UpgradeFrom(1) is given more than once") {
		t.Fatalf("Register(duplicate UpgradeFrom) error = %v", err)
	}
}

// TestRunChecksTheEnvelope: the envelope invariant holds on the path that
// runs handlers, not only in UnmarshalEnvelope.
func TestRunChecksTheEnvelope(t *testing.T) {
	reg := newRegistry()
	called := false
	jobs.MustRegister(reg, func(context.Context, sendWelcome) error { called = true; return nil })
	for _, env := range []jobs.Envelope{
		{Name: "send_welcome_email", Version: 1, Payload: json.RawMessage(`{"user_id":1}`)},
		{ID: "x", Version: 1, Payload: json.RawMessage(`{"user_id":1}`)},
	} {
		if err := reg.Run(context.Background(), env); !errors.Is(err, jobs.ErrDecode) {
			t.Fatalf("Run(%+v) error = %v, want decode", env, err)
		}
	}
	if called {
		t.Fatal("the handler ran on an envelope without an ID or name")
	}
}
