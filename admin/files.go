package admin

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/claims"
	"github.com/gombit-dev/gombit/storage/filefield"
	"github.com/gombit-dev/gombit/storage/upload"
	"github.com/gombit-dev/gombit/types"
)

// Storage-backed fields (types.File, types.Image) in the admin: a row
// carries a file object (filefield.FileInfo: key, filename, size, type,
// URL); a write takes the key of an upload the admin granted (the upload
// endpoint, which claims it: storage/claims). A changed key is accepted
// only when it passes the field's policy, and the write's transaction
// holds its claim (so another record's file, or an expired upload, is
// refused) and releases the claims of the files it replaced, cleared, or
// took with it on delete; those are deleted once the change has committed.
// Files the record holds are never deleted otherwise, and a file without
// a claim (stored before the app adopted claims) is never deleted.

// storer is a Host that has object storage (framework.App does).
type storer interface{ Storage() storage.Storage }

func (h *handlers) store() storage.Storage {
	if s, ok := h.host.(storer); ok {
		return s.Storage()
	}
	return nil
}

// logged is a Host with a logger (framework.App is).
type logged interface{ Logger() *zap.Logger }

// fileClaims is the claims the admin owns files through, over db and the
// host's store; nil when the host has no store.
func (h *handlers) fileClaims(db *gorm.DB) *claims.Claims {
	store := h.store()
	if store == nil {
		return nil
	}
	log := zap.NewNop()
	if l, ok := h.host.(logged); ok && l.Logger() != nil {
		log = l.Logger()
	}
	return filefield.Claims(db, store, log)
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
// upload that passes the field's policy. (A refused file is deleted only
// while no record holds it.) Failures are field errors, the way applyWrite
// reports them.
func (h *handlers) acceptFiles(ctx context.Context, cl *claims.Claims, m *registered, inst any, before map[string]string) error {
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
		if store == nil || cl == nil {
			return contract.WithContext(ctx, contract.Internal("admin: file storage is not attached"))
		}
		err := filefield.Accept(ctx, store, cl, key, *f.policy)
		switch {
		case err == nil:
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

// fileChanges is what a write does to a record's files: the keys it now
// holds that it did not (hold), and the keys it held that it no longer
// does (release). before is nil on create; inst is nil on delete.
func (m *registered) fileChanges(before map[string]string, inst any) (hold, release []string) {
	for _, f := range m.fileFields() {
		old, cur := before[f.Name], ""
		if inst != nil {
			cur = fileKey(f.get(inst))
		}
		if cur == old {
			continue
		}
		if cur != "" {
			hold = append(hold, cur)
		}
		if old != "" {
			release = append(release, old)
		}
	}
	return hold, release
}

// writeFiles runs write (the record's change, in tx) with the record's file
// claims moved in the same transaction (claims.Update): the new keys held,
// the replaced ones released and, once it commits, deleted. Without files
// (or a store) it is write on db.
func (h *handlers) writeFiles(ctx context.Context, cl *claims.Claims, db *gorm.DB, m *registered, before map[string]string, inst any, write func(tx *gorm.DB) error) error {
	if len(m.fileFields()) == 0 || cl == nil {
		return write(db)
	}
	hold, release := m.fileChanges(before, inst)
	err := cl.Update(ctx, hold, release, write)
	var ke *claims.KeyError
	if errors.As(err, &ke) && errors.Is(err, claims.ErrNotPending) {
		// A new key the transaction could not hold: name its field.
		for _, f := range m.fileFields() {
			if inst != nil && fileKey(f.get(inst)) == ke.Key {
				return contract.WithContext(ctx, contract.Validation("The request contains invalid fields.",
					map[string][]string{f.Name: {"is attached to another record, or its upload has expired"}}))
			}
		}
	}
	return err
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
	db, err := h.db()
	if err != nil {
		return nil, contract.WithContext(ctx, contract.Internal("admin database is not attached"))
	}
	store, cl := h.store(), h.fileClaims(db)
	if store == nil {
		return nil, contract.WithContext(ctx, contract.Internal("admin: file storage is not attached"))
	}
	g, err := filefield.Authorize(ctx, store, cl, *f.policy, input.Body)
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
