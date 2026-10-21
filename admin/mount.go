package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
)

const cookieSecurityName = "cookieAuth"

type queryValuesKey struct{}

type handlers struct {
	reg  *registry
	host Host
}

// Mount registers the admin Huma routes on host.API() when cookie auth is on.
// The catalog is empty until Register is called. framework.New calls Mount
// automatically in cookie mode, with the app's auth options (its Auditor,
// which records the admin's writes); JWT-only apps must not call it.
func Mount(host Host, opts ...auth.Option) error {
	if host == nil {
		return errors.New("admin: nil host")
	}
	if host.API() == nil {
		return errors.New("admin: nil API")
	}
	cfg := host.Config()
	if !cfg.Auth.Enabled() || cfg.Auth.EffectiveMode() != config.AuthModeCookie {
		return nil
	}
	if host.DB() == nil {
		return errors.New("admin: nil database")
	}
	svc, err := auth.NewService(host.DB(), cfg, opts...)
	if err != nil {
		return err
	}
	reg := newRegistry()
	if !storeRegistry(host.API(), reg) {
		return errors.New("admin: already mounted")
	}
	mountRoutes(host, reg, svc)
	return nil
}

func mountRoutes(host Host, reg *registry, svc *auth.Service) {
	prefix := strings.TrimSuffix(host.Config().API.Prefix, "/")
	if prefix == "" {
		prefix = "/api/v1"
	}
	h := &handlers{reg: reg, host: host}
	gates := huma.Middlewares{auth.CaptureRequestInfo(), svc.RequireCookieSession(), attachQuery}
	security := []map[string][]string{{cookieSecurityName: {}}}
	tags := []string{"Admin"}

	huma.Register(host.API(), huma.Operation{
		OperationID: "admin-meta-list",
		Method:      http.MethodGet,
		Path:        prefix + "/admin/meta",
		Summary:     "List registered admin models",
		Tags:        tags,
		Security:    security,
		Middlewares: gates,
	}, h.listMeta)

	huma.Register(host.API(), huma.Operation{
		OperationID: "admin-meta-get",
		Method:      http.MethodGet,
		Path:        prefix + "/admin/meta/{slug}",
		Summary:     "Get one registered admin model",
		Tags:        tags,
		Security:    security,
		Middlewares: gates,
	}, h.getMeta)

	huma.Register(host.API(), huma.Operation{
		OperationID: "admin-resource-list",
		Method:      http.MethodGet,
		Path:        prefix + "/admin/resources/{slug}",
		Summary:     "List admin resources",
		Tags:        tags,
		Security:    security,
		Middlewares: gates,
	}, h.listResources)

	huma.Register(host.API(), huma.Operation{
		OperationID: "admin-resource-create",
		Method:      http.MethodPost,
		Path:        prefix + "/admin/resources/{slug}",
		Summary:     "Create an admin resource",
		Tags:        tags,
		Security:    security,
		Middlewares: gates,
	}, auditedWrite(svc, auth.AuditAdminCreated, h.createResource, func(in *writeInput, out *rowOutput) (string, string) {
		return in.Slug, h.pkOf(in.Slug, out)
	}))

	huma.Register(host.API(), huma.Operation{
		OperationID: "admin-resource-get",
		Method:      http.MethodGet,
		Path:        prefix + "/admin/resources/{slug}/{id}",
		Summary:     "Get an admin resource",
		Tags:        tags,
		Security:    security,
		Middlewares: gates,
	}, h.getResource)

	huma.Register(host.API(), huma.Operation{
		OperationID: "admin-resource-update",
		Method:      http.MethodPatch,
		Path:        prefix + "/admin/resources/{slug}/{id}",
		Summary:     "Update an admin resource",
		Tags:        tags,
		Security:    security,
		Middlewares: gates,
	}, auditedWrite(svc, auth.AuditAdminUpdated, h.updateResource, func(in *patchInput, _ *rowOutput) (string, string) {
		return in.Slug, in.ID
	}))

	huma.Register(host.API(), huma.Operation{
		OperationID: "admin-resource-delete",
		Method:      http.MethodDelete,
		Path:        prefix + "/admin/resources/{slug}/{id}",
		Summary:     "Delete an admin resource",
		Tags:        tags,
		Security:    security,
		Middlewares: gates,
	}, auditedWrite(svc, auth.AuditAdminDeleted, h.deleteResource, func(in *itemInput, _ *deleteOutput) (string, string) {
		return in.Slug, in.ID
	}))

	mountFileRoutes(host.API(), h, prefix, gates, security, tags)
}

func attachQuery(ctx huma.Context, next func(huma.Context)) {
	u := ctx.URL()
	next(huma.WithValue(ctx, queryValuesKey{}, u.Query()))
}

func queryValues(ctx interface{ Value(any) any }) url.Values {
	v, _ := ctx.Value(queryValuesKey{}).(url.Values)
	return v
}

// auditedWrite records a successful admin write in the security audit
// trail (AUTH-7), through the Service's Auditor: the admin who made it (the
// session's user), the model's slug and the record's primary key, and the
// request. The write has committed by then, so a failure to record it is
// logged, and is not the request's.
func auditedWrite[I, O any](svc *auth.Service, action auth.AuditAction, handler func(context.Context, *I) (*O, error), target func(*I, *O) (slug, id string)) func(context.Context, *I) (*O, error) {
	return func(ctx context.Context, in *I) (*O, error) {
		out, err := handler(ctx, in)
		if err != nil {
			return out, err
		}
		slug, id := target(in, out)
		svc.Audit(ctx, auth.AuditEvent{Action: action, TargetType: slug, TargetID: id})
		return out, nil
	}
}

// pkOf is the primary key of a written row, as the API shows it.
func (h *handlers) pkOf(slug string, out *rowOutput) string {
	m, ok := h.reg.get(slug)
	if !ok || out == nil {
		return ""
	}
	v, ok := out.Body.Data[m.meta.PK]
	if !ok || v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
