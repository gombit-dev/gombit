package jobs

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the instrumentation scope of job spans.
const tracerName = "github.com/gombit-dev/gombit/jobs"

// OTelPropagator carries OpenTelemetry context (W3C traceparent, tracestate,
// and baggage) from the dispatching context into the handler's, so a job's
// span continues the trace of the request that queued it. It uses the
// application's global propagator when one is set, W3C trace context and
// baggage otherwise. Without an OpenTelemetry SDK in the app there is no span
// context to carry, and it writes nothing.
func OTelPropagator() Propagator { return otelPropagator{} }

type otelPropagator struct{}

func (otelPropagator) textMap() propagation.TextMapPropagator {
	if p := otel.GetTextMapPropagator(); len(p.Fields()) > 0 {
		return p
	}
	return propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
}

func (p otelPropagator) Inject(ctx context.Context, metadata map[string]string) {
	p.textMap().Inject(ctx, propagation.MapCarrier(metadata))
}

func (p otelPropagator) Extract(ctx context.Context, metadata map[string]string) context.Context {
	if len(metadata) == 0 {
		return ctx
	}
	return p.textMap().Extract(ctx, propagation.MapCarrier(metadata))
}

// startRunSpan starts the span of one job run, a child of whatever trace the
// propagators restored. It uses the global tracer provider, a no-op unless
// the application installs an OpenTelemetry SDK.
func startRunSpan(ctx context.Context, name, id string, version, attempt int) (context.Context, trace.Span) {
	return otel.Tracer(tracerName).Start(ctx, "job "+name,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("gombit.job.name", name),
			attribute.String("gombit.job.id", id),
			attribute.Int("gombit.job.version", version),
			attribute.Int("gombit.job.attempt", attempt),
		))
}

// endRunSpan records the run's failure, if any, and ends the span.
func endRunSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, string(Classify(err)))
		span.SetAttributes(attribute.String("gombit.job.failure", string(Classify(err))))
	}
	span.End()
}
