package admin

import (
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gombit-dev/gombit/config"
	"gorm.io/gorm"
)

// Host is satisfied by *framework.App. It is defined here so this package
// does not import framework (framework.New calls Mount).
type Host interface {
	API() huma.API
	DB() *gorm.DB
	Config() config.Config
}

type registry struct {
	mu     sync.RWMutex
	models []*registered
	bySlug map[string]*registered
}

func newRegistry() *registry {
	return &registry{bySlug: map[string]*registered{}}
}

func (r *registry) add(m *registered) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.bySlug[m.meta.Slug]; ok {
		return errDuplicateSlug(m.meta.Slug)
	}
	r.models = append(r.models, m)
	r.bySlug[m.meta.Slug] = m
	return nil
}

func (r *registry) get(slug string) (*registered, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.bySlug[slug]
	return m, ok
}

func (r *registry) all() []*registered {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*registered, len(r.models))
	copy(out, r.models)
	return out
}

type registered struct {
	meta        ModelMeta
	actions     Actions
	pkColumn    string
	pkType      FieldType
	newInstance func() any
	newSlice    func() any
	forEach     func(slicePtr any, fn func(any))
	fields      []resolvedField
	fieldByName map[string]*resolvedField
	implicit    map[string]implicitColumn // created_at / updated_at when omitted from Fields
	m2m         []*m2mBinding             // many-to-many fields synced on write (#223)
	hasMany     []*relationRead           // has_many fields, read-only (preload + ids) (#223)
	version     *versionField             // optimistic-lock column, if the model has one
}

// versionField describes a model's integer optimistic-locking column (named
// "version"). When present, the admin update guards the write on the version and
// bumps it, so concurrent PATCHes cannot silently last-write-wins (#224).
type versionField struct {
	name   string // json field name used in the request body
	column string // DB column
	get    func(inst any) int64
	set    func(inst any, v int64)
}

type resolvedField struct {
	Field
	column  string
	pointer bool
	get     func(inst any) any
	set     func(inst any, raw any) error
}

// implicitColumn is a GORM timestamp allowed in List/Ordering without a Field.
type implicitColumn struct {
	column string
	get    func(inst any) any
}

func (m *registered) field(name string) (*resolvedField, bool) {
	f, ok := m.fieldByName[name]
	return f, ok
}

func (m *registered) columnFor(name string) (string, bool) {
	if f, ok := m.fieldByName[name]; ok {
		if f.column == "" {
			return "", false
		}
		return f.column, true
	}
	if col, ok := m.implicit[name]; ok && col.column != "" {
		return col.column, true
	}
	return "", false
}

func (m *registered) toRow(inst any) row {
	out := make(row, len(m.fields)+len(m.implicit))
	for i := range m.fields {
		f := &m.fields[i]
		if f.WriteOnly {
			continue
		}
		out[f.Name] = f.get(inst)
	}
	for name, col := range m.implicit {
		if _, exists := out[name]; exists {
			continue
		}
		if col.get == nil {
			continue
		}
		out[name] = col.get(inst)
	}
	return out
}

var registries sync.Map // huma.API -> *registry

func registryFor(api huma.API) (*registry, bool) {
	if api == nil {
		return nil, false
	}
	v, ok := registries.Load(api)
	if !ok {
		return nil, false
	}
	reg, ok := v.(*registry)
	return reg, ok
}

func storeRegistry(api huma.API, reg *registry) bool {
	_, loaded := registries.LoadOrStore(api, reg)
	return !loaded
}
