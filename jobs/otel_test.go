package jobs_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/gombit-dev/gombit/jobs"
)

// TestJobSpansContinueTheDispatchingTrace: the job's span is a child of the
// span that dispatched it, across the envelope.
func TestJobSpansContinueTheDispatchingTrace(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	reg := jobs.NewRegistry(jobs.WithPropagator(jobs.OTelPropagator()))
	fail := false
	jobs.MustRegister(reg, func(context.Context, sendWelcome) error {
		if fail {
			return errors.New("smtp down")
		}
		return nil
	})
	ctx, request := provider.Tracer("test").Start(context.Background(), "POST /signup")
	env, err := reg.Encode(ctx, sendWelcome{UserID: 1})
	request.End()
	if err != nil {
		t.Fatal(err)
	}
	if env.Metadata["traceparent"] == "" {
		t.Fatalf("metadata = %v, want a traceparent", env.Metadata)
	}
	// The worker runs it later, on a context with no trace of its own.
	if err := reg.Run(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	fail = true
	env.Attempt = 2
	_ = reg.Run(context.Background(), env)

	spans := recorder.Ended()
	if len(spans) != 3 {
		t.Fatalf("recorded %d spans, want the request and two job runs", len(spans))
	}
	parent := request.SpanContext()
	for _, s := range spans[1:] {
		if s.Name() != "job send_welcome_email" || s.Parent().SpanID() != parent.SpanID() || s.SpanContext().TraceID() != parent.TraceID() {
			t.Fatalf("span %q parent %s trace %s, want a child of the dispatching span", s.Name(), s.Parent().SpanID(), s.SpanContext().TraceID())
		}
	}
	if spans[1].Status().Code == codes.Error || spans[2].Status().Code != codes.Error {
		t.Fatalf("statuses = %v, %v; want ok then error", spans[1].Status(), spans[2].Status())
	}
}

func TestOTelPropagatorWithoutATraceWritesNothing(t *testing.T) {
	md := map[string]string{}
	jobs.OTelPropagator().Inject(context.Background(), md)
	if len(md) != 0 {
		t.Fatalf("metadata = %v, want nothing without a span", md)
	}
	if ctx := jobs.OTelPropagator().Extract(context.Background(), nil); ctx == nil {
		t.Fatal("Extract returned nil")
	}
}

// TestSpansCoverFailuresBeforeTheHandler: a run that fails decoding or
// panics is still a span, with an error status.
func TestSpansCoverFailuresBeforeTheHandler(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	reg := jobs.NewRegistry()
	jobs.MustRegister(reg, func(context.Context, sendWelcome) error { panic("boom") })
	undecodable := jobs.Envelope{ID: "x", Name: "send_welcome_email", Version: 1, Payload: []byte(`{"user_id":"seven"}`)}
	if err := reg.Run(context.Background(), undecodable); !errors.Is(err, jobs.ErrDecode) {
		t.Fatalf("Run(undecodable) = %v", err)
	}
	env, _ := reg.Encode(context.Background(), sendWelcome{})
	if err := reg.Run(context.Background(), env); !errors.Is(err, jobs.ErrPanic) {
		t.Fatalf("Run(panicking) = %v", err)
	}
	spans := recorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("recorded %d spans, want one per run", len(spans))
	}
	for i, want := range []string{"decode", "panic"} {
		if spans[i].Status().Code != codes.Error || spans[i].Status().Description != want {
			t.Errorf("span %d status = %+v, want an error (%s)", i, spans[i].Status(), want)
		}
	}
}
