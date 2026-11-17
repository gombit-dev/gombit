package contract

import "github.com/danielgtaylor/huma/v2"

// humaDefaultBodyBytes is Huma's per-operation default request body limit
// (1 MiB), applied by huma.Register to any operation that leaves MaxBodyBytes
// unset.
const humaDefaultBodyBytes int64 = 1024 * 1024

// WithBodyLimit returns api with Huma's default per-operation body limit raised
// to maxBytes. Huma applies its 1 MiB default when an operation is registered;
// this wrapper rewrites that default at registration time, so every typed
// operation (generated resources, contract routes, admin, auth) shares the
// framework's JSON cap. An operation that sets its own MaxBodyBytes to a value
// other than Huma's 1 MiB default keeps it.
func WithBodyLimit(api huma.API, maxBytes int64) huma.API {
	return bodyLimitAPI{API: api, maxBytes: maxBytes}
}

type bodyLimitAPI struct {
	huma.API
	maxBytes int64
}

func (a bodyLimitAPI) Adapter() huma.Adapter {
	return bodyLimitAdapter{Adapter: a.API.Adapter(), maxBytes: a.maxBytes}
}

type bodyLimitAdapter struct {
	huma.Adapter
	maxBytes int64
}

func (a bodyLimitAdapter) Handle(op *huma.Operation, handler func(ctx huma.Context)) {
	if op.MaxBodyBytes == humaDefaultBodyBytes {
		op.MaxBodyBytes = a.maxBytes
	}
	a.Adapter.Handle(op, handler)
}
