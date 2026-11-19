package contract

import (
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
)

// recordingAdapter captures the operation handed to Handle so the test can
// read the MaxBodyBytes the wrapper left on it.
type recordingAdapter struct {
	huma.Adapter
	seen []*huma.Operation
}

func (a *recordingAdapter) Handle(op *huma.Operation, _ func(ctx huma.Context)) {
	a.seen = append(a.seen, op)
}

func TestBodyLimitRaisesHumaDefaultToCap(t *testing.T) {
	t.Parallel()

	const capBytes int64 = 8 << 20
	inner := &recordingAdapter{}
	adapter := bodyLimitAdapter{Adapter: inner, maxBytes: capBytes}

	op := &huma.Operation{MaxBodyBytes: humaDefaultBodyBytes}
	adapter.Handle(op, func(huma.Context) {})

	if len(inner.seen) != 1 {
		t.Fatalf("inner adapter saw %d operations, want 1", len(inner.seen))
	}
	if got := inner.seen[0].MaxBodyBytes; got != capBytes {
		t.Fatalf("MaxBodyBytes = %d, want %d (Huma default raised to the cap)", got, capBytes)
	}
}

func TestBodyLimitKeepsOperationOwnLimit(t *testing.T) {
	t.Parallel()

	const capBytes int64 = 8 << 20
	const ownBytes int64 = 3 << 20
	inner := &recordingAdapter{}
	adapter := bodyLimitAdapter{Adapter: inner, maxBytes: capBytes}

	op := &huma.Operation{MaxBodyBytes: ownBytes}
	adapter.Handle(op, func(huma.Context) {})

	if got := inner.seen[0].MaxBodyBytes; got != ownBytes {
		t.Fatalf("MaxBodyBytes = %d, want %d (an explicit per-operation limit is kept)", got, ownBytes)
	}
}

func TestBodyLimitKeepsUnlimitedAndMultipartLimits(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		max  int64
	}{
		{name: "unlimited", max: -1},
		{name: "multipart without a default", max: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inner := &recordingAdapter{}
			adapter := bodyLimitAdapter{Adapter: inner, maxBytes: 8 << 20}
			op := &huma.Operation{MaxBodyBytes: tc.max}
			adapter.Handle(op, func(huma.Context) {})

			if got := inner.seen[0].MaxBodyBytes; got != tc.max {
				t.Fatalf("MaxBodyBytes = %d, want %d", got, tc.max)
			}
		})
	}
}

func TestWithBodyLimitWrapsAdapter(t *testing.T) {
	t.Parallel()

	_, api := humatest.New(t)
	const capBytes int64 = 8 << 20
	wrapped := WithBodyLimit(api, capBytes)

	adapter, ok := wrapped.Adapter().(bodyLimitAdapter)
	if !ok {
		t.Fatalf("Adapter() = %T, want bodyLimitAdapter", wrapped.Adapter())
	}
	if adapter.maxBytes != capBytes {
		t.Fatalf("maxBytes = %d, want %d", adapter.maxBytes, capBytes)
	}
}

func TestWithBodyLimitForwardsConfig(t *testing.T) {
	t.Parallel()

	config := huma.DefaultConfig("test", "1.0.0")
	config.RejectUnknownQueryParameters = true
	_, api := humatest.New(t, config)
	wrapped := WithBodyLimit(api, 8<<20)

	provider, ok := wrapped.(interface{ Config() huma.Config })
	if !ok {
		t.Fatalf("wrapped API %T does not expose Huma Config", wrapped)
	}
	if got := provider.Config().RejectUnknownQueryParameters; !got {
		t.Fatal("wrapped API lost RejectUnknownQueryParameters")
	}
	if got := huma.NewGroup(wrapped).Config().RejectUnknownQueryParameters; !got {
		t.Fatal("Huma group did not receive the wrapped API config")
	}
}

func TestCodeForHTTPStatusMapsRequestEntityTooLarge(t *testing.T) {
	t.Parallel()

	if got := codeForHTTPStatus(http.StatusRequestEntityTooLarge); got != CodePayloadTooLarge {
		t.Fatalf("codeForHTTPStatus(413) = %q, want %q", got, CodePayloadTooLarge)
	}
	code, _ := classifyError(http.StatusRequestEntityTooLarge, "request body is too large limit=1048576 bytes")
	if code != CodePayloadTooLarge {
		t.Fatalf("classifyError(413) code = %q, want %q", code, CodePayloadTooLarge)
	}
}
