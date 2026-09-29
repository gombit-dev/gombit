package admin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/filefield"
	"github.com/gombit-dev/gombit/storage/upload"
	"github.com/gombit-dev/gombit/types"
)

// Storage-backed fields (types.File, types.Image) in the admin: a row
// carries a file object (filefield.FileInfo: key, filename, size, type,
// URL); a write takes the key of an upload the admin granted (the upload
// endpoint); a changed key is accepted only when it passes the field's
// policy and no other record holds it; and a file the record replaced,
// cleared, or took with it on delete is deleted once the change has
// committed, and only under the prefix the field owns.

// storer is a Host that has object storage (framework.App does).
type storer interface{ Storage() storage.Storage }

func (h *handlers) store() storage.Storage {
	if s, ok := h.host.(storer); ok {
		return s.Storage()
	}
	return nil
}

// fileFields are m's storage-backed fields (found at registration).
func (m *registered) fileFields() []*resolvedField { return m.files }

// fileKey is the key a file field's getter returned ("" for none).
func fileKey(v any) string {
	switch v := v.(type) {
	case types.File:
		return string(v)
	case types.Image:
		return string(v)
	case string:
		return v
	default:
		return ""
	}
}

// fileKeys returns the key of each file field of inst.
func (m *registered) fileKeys(inst any) map[string]string {
	keys := map[string]string{}
	for _, f := range m.fileFields() {
		keys[f.Name] = fileKey(f.get(inst))
	}
	return keys
}

// resolveFiles replaces each file field's key in r with its file object.
func (h *handlers) resolveFiles(ctx context.Context, m *registered, r row) error {
	files := m.fileFields()
	if len(files) == 0 {
		return nil
	}
	store := h.store()
	for _, f := range files {
		if _, ok := r[f.Name]; !ok {
			continue
		}
		key := fileKey(r[f.Name])
		if store == nil || key == "" {
			if key == "" {
				r[f.Name] = nil
			}
			continue
		}
		info, err := filefield.Resolve(ctx, store, key)
		if err != nil {
			if ctx.Err() != nil {
				return filefield.MapError(ctx, err)
			}
			// The store failed (unavailable, say): the row still shows, with
			// the file's key and no details, rather than fail the page.
			info = &filefield.FileInfo{Key: key}
		}
		r[f.Name] = info
	}
	return nil
}

// resolveRows resolves the files of every row, a few rows at a time.
func (h *handlers) resolveRows(ctx context.Context, m *registered, rows []row) error {
	if len(m.fileFields()) == 0 {
		return nil
	}
	return filefield.ForEach(ctx, len(rows), func(ctx context.Context, i int) error {
		return h.resolveFiles(ctx, m, rows[i])
	})
}

// acceptFiles checks every file field whose key a write changed (before
// holds the keys the record had, nil on create): each new key must be an
// upload that passes the field's policy and that no other record holds.
// Failures are field errors, the way applyWrite reports them.
func (h *handlers) acceptFiles(ctx context.Context, db *gorm.DB, m *registered, inst any, before map[string]string) error {
	files := m.fileFields()
	if len(files) == 0 {
		return nil
	}
	store := h.store()
	errs := map[string][]string{}
	for _, f := range files {
		key := fileKey(f.get(inst))
		if key == "" || key == before[f.Name] {
			continue
		}
		if store == nil {
			return contract.WithContext(ctx, contract.Internal("admin: file storage is not attached"))
		}
		err := filefield.Accept(ctx, db, store, m.newInstance(), f.column, key, *f.policy)
		switch {
		case err == nil:
		case errors.Is(err, filefield.ErrReferenced):
			errs[f.Name] = append(errs[f.Name], "is attached to another record")
		case errors.Is(err, upload.ErrNoFile):
			errs[f.Name] = append(errs[f.Name], "was not uploaded")
		case errors.Is(err, upload.ErrTooLarge):
			errs[f.Name] = append(errs[f.Name], "is too large")
		case errors.Is(err, upload.ErrType):
			errs[f.Name] = append(errs[f.Name], "is not an accepted type")
		case errors.Is(err, upload.ErrMalformed):
			errs[f.Name] = append(errs[f.Name], "is not an upload for this field")
		default:
			return filefield.MapError(ctx, err)
		}
	}
	if len(errs) > 0 {
		return contract.WithContext(ctx, contract.Validation("The request contains invalid fields.", errs))
	}
	return nil
}

// discardFiles deletes the files a committed change let go of: each key in
// before that inst no longer holds (all of them when inst is nil, a
// deleted record). Only keys under the field's own prefix are deleted
// (storage.DeleteOwned). A failed delete leaves a file no record refers
// to, which storage.Sweep removes; the change itself has succeeded.
func (h *handlers) discardFiles(ctx context.Context, m *registered, before map[string]string, inst any) {
	store := h.store()
	if store == nil {
		return
	}
	for _, f := range m.fileFields() {
		old := before[f.Name]
		if old == "" {
			continue
		}
		if inst != nil && fileKey(f.get(inst)) == old {
			continue
		}
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_, _ = storage.DeleteOwned(dctx, store, old, f.policy.Prefix)
		cancel()
	}
}

type uploadGrantInput struct {
	Slug  string                       `path:"slug" doc:"Registered model slug"`
	Field string                       `path:"field" doc:"A file or image field of the model"`
	Body  filefield.UploadGrantRequest `doc:"The file to upload"`
}

type uploadGrantOutput struct {
	Body contract.Data[filefield.UploadGrant]
}

// grantUpload grants one direct upload for a file field, to an operator
// who may create or update the model: the admin form uploads the file with
// it, then sends the key.
func (h *handlers) grantUpload(ctx context.Context, input *uploadGrantInput) (*uploadGrantOutput, error) {
	m, ok := h.reg.get(input.Slug)
	if !ok {
		return nil, contract.WithContext(ctx, contract.NotFound("unknown model"))
	}
	var allowed error = contract.WithContext(ctx, contract.Authorization("action disabled"))
	if m.actions.Create {
		allowed = h.requirePermission(ctx, m.meta.Permissions.Create)
	}
	if allowed != nil && m.actions.Update {
		allowed = h.requirePermission(ctx, m.meta.Permissions.Update)
	}
	if allowed != nil {
		return nil, allowed
	}
	f, ok := m.field(input.Field)
	if !ok || f.policy == nil || f.ReadOnly {
		return nil, contract.WithContext(ctx, contract.NotFound("unknown file field"))
	}
	store := h.store()
	if store == nil {
		return nil, contract.WithContext(ctx, contract.Internal("admin: file storage is not attached"))
	}
	g, err := filefield.Authorize(ctx, store, *f.policy, input.Body)
	if err != nil {
		return nil, filefield.MapError(ctx, err)
	}
	return &uploadGrantOutput{Body: contract.Data[filefield.UploadGrant]{Data: g}}, nil
}

// mountFileRoutes registers the upload endpoint.
func mountFileRoutes(api huma.API, h *handlers, prefix string, gates huma.Middlewares, security []map[string][]string, tags []string) {
	huma.Register(api, huma.Operation{
		OperationID: "admin-resource-upload",
		Method:      http.MethodPost,
		Path:        prefix + "/admin/resources/{slug}/uploads/{field}",
		Summary:     "Grant an upload for a file field",
		Tags:        tags,
		Security:    security,
		Middlewares: gates,
	}, h.grantUpload)
}
