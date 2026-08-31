package admin

import (
	"context"
	"errors"
	"strings"

	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/database"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type listInput struct {
	Slug     string `path:"slug" doc:"Registered model slug"`
	Page     int    `query:"page" doc:"1-based page"`
	PerPage  int    `query:"per_page" doc:"Page size"`
	Search   string `query:"search" doc:"Search term applied to Options.Search"`
	Ordering string `query:"ordering" doc:"Field from Options.Ordering; prefix with - for DESC"`
}

type writeInput struct {
	Slug string         `path:"slug" doc:"Registered model slug"`
	Body map[string]any `doc:"Writable field values keyed by registered field names"`
}

type itemInput struct {
	Slug string `path:"slug" doc:"Registered model slug"`
	ID   string `path:"id" doc:"Primary key value"`
}

type patchInput struct {
	Slug string         `path:"slug" doc:"Registered model slug"`
	ID   string         `path:"id" doc:"Primary key value"`
	Body map[string]any `doc:"Writable field values keyed by registered field names"`
}

// row is a registered model's field values keyed by field name. It exists
// so the admin data plane's generic responses have a named type: an
// anonymous map[string]any as a generic type parameter has no
// reflect.Type.Name(), so Huma's DefaultSchemaNamer falls back to Go's
// unnamed-type string ("map[string]interface {}") and that literal space
// and braces survive into the OpenAPI component name — producing e.g.
// "DataMetaListMapStringInterface {}PageMeta", which fails OpenAPI 3.1
// validation (component names must match ^[a-zA-Z0-9._-]+$).
type row map[string]any

type rowOutput struct {
	Body contract.Data[row]
}

type listOutput struct {
	Body contract.DataMeta[[]row, contract.PageMeta]
}

type deleteResult struct {
	OK bool `json:"ok" example:"true"`
}

type deleteOutput struct {
	Body contract.Data[deleteResult]
}

func (h *handlers) listResources(ctx context.Context, input *listInput) (*listOutput, error) {
	m, err := h.modelForAction(
		ctx,
		input.Slug,
		func(a Actions) bool { return a.List },
		func(p Permissions) string { return p.View },
	)
	if err != nil {
		return nil, err
	}
	db, err := h.db()
	if err != nil {
		return nil, contract.WithContext(ctx, contract.Internal("admin database is not attached"))
	}

	page, perPage := contract.ClampPage(input.Page, input.PerPage)

	q := db.WithContext(ctx).Model(m.newInstance())
	q, err = applySearch(q, m, input.Search)
	if err != nil {
		return nil, contract.WithContext(ctx, contract.Validation("The request contains invalid fields.", map[string][]string{
			"search": {err.Error()},
		}))
	}
	q, err = applyFilters(ctx, q, m, queryValues(ctx))
	if err != nil {
		return nil, err
	}

	var total int64
	if err := q.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, contract.WithContext(ctx, contract.Internal("count resources"))
	}

	q, err = applyOrdering(q, m, input.Ordering)
	if err != nil {
		return nil, contract.WithContext(ctx, contract.Validation("The request contains invalid fields.", map[string][]string{
			"ordering": {err.Error()},
		}))
	}

	slice := m.newSlice()
	if err := withM2MPreloads(q, m).Offset(contract.PageOffset(page, perPage)).Limit(perPage).Find(slice).Error; err != nil {
		return nil, contract.WithContext(ctx, contract.Internal("list resources"))
	}

	rows := make([]row, 0)
	m.forEach(slice, func(item any) {
		rows = append(rows, m.toRow(item))
	})
	return &listOutput{
		Body: contract.DataMeta[[]row, contract.PageMeta]{
			Data: rows,
			Meta: &contract.PageMeta{Page: page, PerPage: perPage, Total: total},
		},
	}, nil
}

func (h *handlers) createResource(ctx context.Context, input *writeInput) (*rowOutput, error) {
	m, err := h.modelForAction(
		ctx,
		input.Slug,
		func(a Actions) bool { return a.Create },
		func(p Permissions) string { return p.Create },
	)
	if err != nil {
		return nil, err
	}
	db, err := h.db()
	if err != nil {
		return nil, contract.WithContext(ctx, contract.Internal("admin database is not attached"))
	}
	inst := m.newInstance()
	m2mIDs, body, err := splitM2M(ctx, m, input.Body)
	if err != nil {
		return nil, err
	}
	if err := applyWrite(ctx, m, inst, body, true); err != nil {
		return nil, err
	}
	if err := persistWithM2M(ctx, db, m, inst, m2mIDs, true); err != nil {
		return nil, err
	}
	return &rowOutput{Body: contract.Data[row]{Data: rowWithM2M(m, inst, m2mIDs)}}, nil
}

func (h *handlers) getResource(ctx context.Context, input *itemInput) (*rowOutput, error) {
	m, err := h.modelForAction(
		ctx,
		input.Slug,
		func(a Actions) bool { return a.Detail },
		func(p Permissions) string { return p.View },
	)
	if err != nil {
		return nil, err
	}
	inst, err := h.loadByID(ctx, m, input.ID)
	if err != nil {
		return nil, err
	}
	return &rowOutput{Body: contract.Data[row]{Data: m.toRow(inst)}}, nil
}

func (h *handlers) updateResource(ctx context.Context, input *patchInput) (*rowOutput, error) {
	m, err := h.modelForAction(
		ctx,
		input.Slug,
		func(a Actions) bool { return a.Update },
		func(p Permissions) string { return p.Update },
	)
	if err != nil {
		return nil, err
	}
	inst, err := h.loadByID(ctx, m, input.ID)
	if err != nil {
		return nil, err
	}
	db, err := h.db()
	if err != nil {
		return nil, contract.WithContext(ctx, contract.Internal("admin database is not attached"))
	}
	// A model with an optimistic-lock version column takes the version-guarded
	// path. Register refuses a model that has both a version column and m2m
	// fields, so this path never needs to sync a join table.
	if m.version != nil {
		return h.updateVersioned(ctx, m, inst, input, db)
	}
	m2mIDs, body, err := splitM2M(ctx, m, input.Body)
	if err != nil {
		return nil, err
	}
	if err := applyWrite(ctx, m, inst, body, false); err != nil {
		return nil, err
	}
	if err := persistWithM2M(ctx, db, m, inst, m2mIDs, false); err != nil {
		return nil, err
	}
	return &rowOutput{Body: contract.Data[row]{Data: rowWithM2M(m, inst, m2mIDs)}}, nil
}

// updateVersioned performs an optimistic-locking update for a model that carries
// an integer "version" column. The guard is the version read when the row was
// loaded, or a version supplied in the PATCH body when present (a stricter
// precondition based on what the client last saw). The UPDATE bumps the version
// and matches on the old value, so two concurrent PATCHes cannot both win: the
// loser matches zero rows and gets a 409 instead of a silent last-write-wins.
func (h *handlers) updateVersioned(ctx context.Context, m *registered, inst any, input *patchInput, db *gorm.DB) (*rowOutput, error) {
	expected := m.version.get(inst)
	body := input.Body
	if raw, ok := body[m.version.name]; ok {
		cv, err := asInt64(raw)
		if err != nil {
			return nil, contract.WithContext(ctx, contract.Validation(
				"The request contains invalid fields.",
				map[string][]string{m.version.name: {"must be an integer"}},
			))
		}
		expected = cv
		body = withoutKey(body, m.version.name)
	}
	if err := applyWrite(ctx, m, inst, body, false); err != nil {
		return nil, err
	}
	m.version.set(inst, expected+1)
	res := db.WithContext(ctx).
		Model(inst).
		Where(clause.Eq{Column: clause.Column{Name: m.version.column}, Value: expected}).
		Select("*").
		Updates(inst)
	if res.Error != nil {
		return nil, database.MapPersistError(ctx, res.Error, "resource already exists", "persist resource")
	}
	if res.RowsAffected == 0 {
		return nil, contract.WithContext(ctx, contract.Conflict(
			"The resource was modified by another request; reload and retry."))
	}
	return &rowOutput{Body: contract.Data[row]{Data: m.toRow(inst)}}, nil
}

// withoutKey returns a shallow copy of body without key.
func withoutKey(body map[string]any, key string) map[string]any {
	out := make(map[string]any, len(body))
	for k, v := range body {
		if k == key {
			continue
		}
		out[k] = v
	}
	return out
}

func (h *handlers) deleteResource(ctx context.Context, input *itemInput) (*deleteOutput, error) {
	m, err := h.modelForAction(
		ctx,
		input.Slug,
		func(a Actions) bool { return a.Delete },
		func(p Permissions) string { return p.Delete },
	)
	if err != nil {
		return nil, err
	}
	inst, err := h.loadByID(ctx, m, input.ID)
	if err != nil {
		return nil, err
	}
	db, err := h.db()
	if err != nil {
		return nil, contract.WithContext(ctx, contract.Internal("admin database is not attached"))
	}
	if err := db.WithContext(ctx).Delete(inst).Error; err != nil {
		return nil, database.MapDeleteError(ctx, err, "resource is still referenced by other records", "delete resource")
	}
	return &deleteOutput{Body: contract.Data[deleteResult]{Data: deleteResult{OK: true}}}, nil
}

func (h *handlers) modelForAction(
	ctx context.Context,
	slug string,
	enabled func(Actions) bool,
	permission func(Permissions) string,
) (*registered, error) {
	m, ok := h.reg.get(slug)
	if !ok {
		return nil, contract.WithContext(ctx, contract.NotFound("unknown model"))
	}
	if !enabled(m.actions) {
		return nil, contract.WithContext(ctx, contract.Authorization("action disabled"))
	}
	if err := h.requirePermission(ctx, permission(m.meta.Permissions)); err != nil {
		return nil, err
	}
	return m, nil
}

func (h *handlers) loadByID(ctx context.Context, m *registered, id string) (any, error) {
	db, err := h.db()
	if err != nil {
		return nil, contract.WithContext(ctx, contract.Internal("admin database is not attached"))
	}
	pk, err := coercePathID(id, m.pkType)
	if err != nil {
		return nil, contract.WithContext(ctx, contract.NotFound("unknown resource"))
	}
	inst := m.newInstance()
	q := withM2MPreloads(db.WithContext(ctx), m)
	err = q.Where(clause.Eq{Column: clause.Column{Name: m.pkColumn}, Value: pk}).First(inst).Error
	if err != nil {
		return nil, database.MapLoadError(ctx, err, "unknown resource", "load resource")
	}
	return inst, nil
}

// withM2MPreloads preloads every many-to-many association so toRow can read the
// related primary keys off the loaded rows.
func withM2MPreloads(q *gorm.DB, m *registered) *gorm.DB {
	for _, b := range m.m2m {
		q = q.Preload(b.assoc)
	}
	for _, b := range m.hasMany {
		q = q.Preload(b.assoc)
	}
	return q
}

// splitM2M separates many-to-many id lists (validated against the related
// primary key type) from the scalar body applyWrite handles.
func splitM2M(ctx context.Context, m *registered, body map[string]any) (ids map[string][]any, rest map[string]any, err error) {
	ids = map[string][]any{}
	if len(m.m2m) == 0 {
		return ids, body, nil
	}
	byName := make(map[string]*m2mBinding, len(m.m2m))
	for _, b := range m.m2m {
		byName[b.name] = b
	}
	rest = make(map[string]any, len(body))
	for k, v := range body {
		b, ok := byName[k]
		if !ok {
			rest[k] = v
			continue
		}
		// A read-only m2m field is not writable — enforce it here, because the
		// id list is split out before applyWrite (which is where the read-only
		// 422 normally happens for scalar fields).
		if f, ok := m.field(k); ok && f.ReadOnly {
			return nil, nil, contract.WithContext(ctx, contract.Validation(
				"The request contains invalid fields.",
				map[string][]string{k: {"field is read-only"}},
			))
		}
		coerced, cerr := coerceM2MIDs(v, b.relatedPKType)
		if cerr != nil {
			return nil, nil, contract.WithContext(ctx, contract.Validation(
				"The request contains invalid fields.",
				map[string][]string{k: {cerr.Error()}},
			))
		}
		ids[k] = coerced
	}
	return ids, rest, nil
}

// persistWithM2M writes the base row and syncs the many-to-many join tables in
// a single transaction, so a bad related id (a 422 from the sync) rolls back the
// parent insert/update instead of leaving an orphan row. A model with no m2m
// fields writes directly (no transaction needed).
func persistWithM2M(ctx context.Context, db *gorm.DB, m *registered, inst any, ids map[string][]any, creating bool) error {
	write := func(tx *gorm.DB) error {
		var perr error
		if creating {
			perr = tx.WithContext(ctx).Create(inst).Error
		} else {
			perr = tx.WithContext(ctx).Save(inst).Error
		}
		if perr != nil {
			return database.MapPersistError(ctx, perr, "resource already exists", "persist resource")
		}
		return syncM2M(ctx, tx, m, inst, ids)
	}
	if len(m.m2m) == 0 {
		return write(db)
	}
	return db.WithContext(ctx).Transaction(write)
}

// syncM2M syncs each submitted many-to-many association's join table. Fields
// omitted from the request are left untouched (partial update).
func syncM2M(ctx context.Context, db *gorm.DB, m *registered, inst any, ids map[string][]any) error {
	for _, b := range m.m2m {
		vals, ok := ids[b.name]
		if !ok {
			continue
		}
		if err := b.sync(ctx, db, inst, vals); err != nil {
			return err
		}
	}
	return nil
}

// rowWithM2M builds the response row, overwriting synced relation fields with
// the ids just written (the loaded instance's association slice is stale after
// a sync).
func rowWithM2M(m *registered, inst any, ids map[string][]any) row {
	out := m.toRow(inst)
	for name, vals := range ids {
		list := make([]any, len(vals))
		copy(list, vals)
		out[name] = list
	}
	return out
}

func (h *handlers) db() (*gorm.DB, error) {
	if h.host == nil || h.host.DB() == nil {
		return nil, errors.New("admin: nil database")
	}
	return h.host.DB(), nil
}

func applyWrite(ctx context.Context, m *registered, inst any, body map[string]any, creating bool) error {
	if body == nil {
		body = map[string]any{}
	}
	fields := map[string][]string{}
	seen := map[string]struct{}{}
	for name, raw := range body {
		f, ok := m.field(name)
		if !ok {
			fields[name] = []string{"unknown field"}
			continue
		}
		if f.ReadOnly || (f.Type == TypeRelation && f.Related != nil && f.Related.Kind == RelHasMany) {
			fields[name] = []string{"field is read-only"}
			continue
		}
		if raw == nil && f.Required {
			fields[name] = []string{"is required"}
			continue
		}
		if err := f.set(inst, raw); err != nil {
			fields[name] = []string{err.Error()}
			continue
		}
		seen[name] = struct{}{}
	}
	if creating {
		for i := range m.fields {
			f := &m.fields[i]
			if !f.Required || f.ReadOnly {
				continue
			}
			if _, ok := seen[f.Name]; ok {
				continue
			}
			if _, ok := fields[f.Name]; ok {
				continue
			}
			fields[f.Name] = []string{"is required"}
		}
	}
	if len(fields) > 0 {
		return contract.WithContext(ctx, contract.Validation("The request contains invalid fields.", fields))
	}
	return nil
}

func applySearch(q *gorm.DB, m *registered, term string) (*gorm.DB, error) {
	term = strings.TrimSpace(term)
	if term == "" {
		return q, nil
	}
	if len(m.meta.Search) == 0 {
		return q, nil
	}
	pattern := "%" + escapeLike(term) + "%"
	ors := make([]clause.Expression, 0, len(m.meta.Search))
	for _, name := range m.meta.Search {
		col, ok := m.columnFor(name)
		if !ok {
			continue
		}
		ors = append(ors, clause.Expr{
			SQL:  "LOWER(" + quoteIdent(q, col) + ") LIKE LOWER(?) ESCAPE ?",
			Vars: []any{pattern, `\`},
		})
	}
	if len(ors) == 0 {
		return q, nil
	}
	return q.Where(clause.Or(ors...)), nil
}

func applyFilters(ctx context.Context, q *gorm.DB, m *registered, values interface{ Get(string) string }) (*gorm.DB, error) {
	if values == nil {
		return q, nil
	}
	for _, name := range m.meta.Filter {
		raw := strings.TrimSpace(values.Get(name))
		if raw == "" {
			continue
		}
		f, ok := m.field(name)
		if !ok || f.column == "" {
			continue
		}
		val, err := coerceFilter(raw, f.Type)
		if err != nil {
			return nil, contract.WithContext(ctx, contract.Validation("The request contains invalid fields.", map[string][]string{
				name: {err.Error()},
			}))
		}
		q = q.Where(clause.Eq{Column: clause.Column{Name: f.column}, Value: val})
	}
	return q, nil
}

func applyOrdering(q *gorm.DB, m *registered, ordering string) (*gorm.DB, error) {
	ordering = strings.TrimSpace(ordering)
	if ordering == "" {
		return q, nil
	}
	desc := false
	name := ordering
	if strings.HasPrefix(name, "-") {
		desc = true
		name = strings.TrimPrefix(name, "-")
	}
	allowed := false
	for _, n := range m.meta.Ordering {
		if n == name {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, errors.New("ordering is not allowed for this field")
	}
	col, ok := m.columnFor(name)
	if !ok {
		return nil, errors.New("ordering is not allowed for this field")
	}
	return q.Order(clause.OrderByColumn{Column: clause.Column{Name: col}, Desc: desc}), nil
}

func quoteIdent(db *gorm.DB, name string) string {
	var b strings.Builder
	db.QuoteTo(&b, name)
	return b.String()
}
