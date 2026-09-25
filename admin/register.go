package admin

import (
	"fmt"
	"strings"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/field"
	"gorm.io/gorm/schema"
)

// Register adds model T to the host's admin registry.
//
// host is typically *framework.App. Missing or duplicate Slug is an error.
// Cookie auth must already be on (framework.New mounts the empty admin
// routes in that mode). After Register returns, the registry holds concrete
// Options values plus constructors for T; request handlers do not walk
// arbitrary Go types.
func Register[T any](host Host, model T, opts Options) error {
	return registerModel(host, model, opts)
}

func registerModel(host Host, model any, opts Options) error {
	if host == nil {
		return fmt.Errorf("admin: nil host")
	}
	cfg := host.Config()
	if !cfg.Auth.Enabled() || cfg.Auth.EffectiveMode() != config.AuthModeCookie {
		return fmt.Errorf("admin: Register requires cookie auth (Auth.Mode=%s)", config.AuthModeCookie)
	}
	reg, ok := registryFor(host.API())
	if !ok {
		return fmt.Errorf("admin: routes are not mounted; Register requires cookie auth")
	}

	opts.Slug = strings.TrimSpace(opts.Slug)
	if opts.Slug == "" {
		return errMissingSlug()
	}
	if !validSlug(opts.Slug) {
		return errInvalidSlug(opts.Slug)
	}

	elem, err := elemTypeOf(model)
	if err != nil {
		return err
	}
	sch, err := parseSchema(model)
	if err != nil {
		return err
	}

	if len(opts.Fields) == 0 {
		derived, err := FieldsFrom(model)
		if err != nil {
			return err
		}
		opts.Fields = derived
	}
	// Default Search to the model's text columns when the caller left it unset
	// (nil), so the admin — and the relation pickers, which search server-side —
	// can filter by name out of the box. A caller who wants no search opts out
	// explicitly with an empty (non-nil) slice.
	if opts.Search == nil {
		opts.Search = defaultSearchFields(opts.Fields)
	}

	if opts.Actions.zero() {
		opts.Actions = defaultActions()
	}
	if opts.Singular == "" {
		if name := elem.Name(); name != "" {
			opts.Singular = name
		} else {
			opts.Singular = titleFromSlug(strings.TrimSuffix(opts.Slug, "s"))
		}
	}
	if opts.Plural == "" {
		if titled := titleFromSlug(opts.Slug); titled != "" {
			opts.Plural = titled
		} else {
			opts.Plural = opts.Singular + "s"
		}
	}
	opts.Permissions = defaultPermissions(opts.Slug, opts.Permissions)

	pkName, pkColumn, pkType, err := derivePK(sch)
	if err != nil {
		return err
	}
	if opts.PK != "" {
		pkName = opts.PK
		if f := fieldByName(opts.Fields, opts.PK); f != nil && f.Column != "" {
			pkColumn = f.Column
		} else if sf := matchSchemaField(sch, Field{Name: opts.PK}); sf != nil {
			pkColumn = sf.DBName
			pkType = inferFieldType(sf)
		}
	}
	if fieldByName(opts.Fields, pkName) == nil {
		return fmt.Errorf("admin: pk %q is not in Fields", pkName)
	}

	implicit := implicitTimestampColumns(sch)
	if err := validateFieldRefs(opts, implicit); err != nil {
		return err
	}

	resolved, m2mBindings, hasManyBindings, err := resolveFields(opts.Fields, sch)
	if err != nil {
		return err
	}
	if err := validateQueryableColumns(opts, resolved); err != nil {
		return err
	}

	m := &registered{
		meta:        modelMetaFrom(opts, pkName),
		actions:     opts.Actions,
		pkColumn:    pkColumn,
		pkType:      pkType,
		newInstance: makeNewInstance(elem),
		newSlice:    makeNewSlice(elem),
		forEach:     makeForEach(elem),
		fields:      resolved,
		fieldByName: map[string]*resolvedField{},
		implicit:    implicit,
		m2m:         m2mBindings,
		hasMany:     hasManyBindings,
	}
	for i := range m.fields {
		m.fieldByName[m.fields[i].Name] = &m.fields[i]
	}
	m.version = detectVersionField(sch)
	// The optimistic-lock update path (updateVersioned) and the many-to-many
	// join sync are separate write paths; combining them on one model would
	// silently drop the m2m write on a versioned PATCH. Refuse the combination
	// at registration rather than accept-and-discard at request time (#223).
	if m.version != nil && len(m.m2m) > 0 {
		return fmt.Errorf("admin: model %q has both a version column and many_to_many field(s), which is not supported yet", opts.Slug)
	}
	if pk, ok := m.fieldByName[pkName]; ok && pk.Type != "" {
		m.pkType = pk.Type
		if pk.column != "" {
			m.pkColumn = pk.column
		}
	}
	return reg.add(m)
}

// defaultSearchFields returns the writable string/text field names of a model,
// the sensible default Search set for a purely auto-registered model.
func defaultSearchFields(fields []Field) []string {
	var out []string
	for _, f := range fields {
		if f.ReadOnly {
			continue
		}
		if f.Type == TypeString || f.Type == TypeText {
			out = append(out, f.Name)
		}
	}
	return out
}

func defaultPermissions(slug string, p Permissions) Permissions {
	if p.View == "" {
		p.View = "admin." + slug + ".view"
	}
	if p.Create == "" {
		p.Create = "admin." + slug + ".create"
	}
	if p.Update == "" {
		p.Update = "admin." + slug + ".update"
	}
	if p.Delete == "" {
		p.Delete = "admin." + slug + ".delete"
	}
	return p
}

func fieldByName(fields []Field, name string) *Field {
	for i := range fields {
		if fields[i].Name == name {
			return &fields[i]
		}
	}
	return nil
}

// fieldAllowsQuery reports whether an admin list option (search, filter,
// ordering) is legal for the field's catalog kind. Relation cardinalities
// read relationCaps; every other admin type is the kind of the same name.
func fieldAllowsQuery(kind string, f Field) bool {
	k := field.Kind(f.Type)
	var rel field.RelationKind
	if f.Type == TypeRelation {
		k = field.Relation
		if f.Related != nil {
			rel = field.RelationKind(f.Related.Kind)
		}
	}
	switch kind {
	case "search":
		return field.AllowsSearch(k, rel)
	case "filter":
		return field.AllowsFilter(k, rel)
	case "ordering":
		return field.AllowsSort(k, rel)
	default:
		return true
	}
}

func validateFieldRefs(opts Options, implicit map[string]implicitColumn) error {
	known := map[string]bool{}
	for _, f := range opts.Fields {
		if f.Name == "" || !validIdent(f.Name) {
			return fmt.Errorf("admin: invalid field name %q", f.Name)
		}
		if f.Column != "" && !validIdent(f.Column) {
			return fmt.Errorf("admin: invalid column %q on field %q", f.Column, f.Name)
		}
		if !validFieldType(f.Type) {
			return fmt.Errorf("admin: field %q has unknown type %q", f.Name, f.Type)
		}
		if f.Type == TypeRelation {
			if f.Related == nil {
				return fmt.Errorf("admin: field %q is a relation without related", f.Name)
			}
			if f.Related.Kind != RelBelongsTo && f.Related.Kind != RelHasMany && f.Related.Kind != RelManyToMany {
				return fmt.Errorf("admin: field %q has unknown relation kind %q", f.Name, f.Related.Kind)
			}
			if strings.TrimSpace(f.Related.Slug) == "" {
				return fmt.Errorf("admin: field %q relation is missing slug", f.Name)
			}
			// A many_to_many id list is split out before applyWrite, so the
			// required-field check there can never see it. Rather than mis-report
			// a submitted list as missing, reject Required on m2m at registration.
			if f.Related.Kind == RelManyToMany && f.Required {
				return fmt.Errorf("admin: many_to_many field %q cannot be Required", f.Name)
			}
		}
		known[f.Name] = true
	}
	check := func(kind string, names []string, allowImplicit bool) error {
		for _, name := range names {
			if known[name] {
				if f := fieldByName(opts.Fields, name); f != nil && !fieldAllowsQuery(kind, *f) {
					detail := string(f.Type)
					if f.Type == TypeRelation && f.Related != nil && f.Related.Kind != "" {
						detail = f.Related.Kind
					}
					return fmt.Errorf("admin: %s %q is %s and cannot be used for %s", kind, name, detail, kind)
				}
				continue
			}
			if allowImplicit && implicitTimestamp(name) {
				if _, ok := implicit[name]; !ok {
					return fmt.Errorf("admin: %s %q is not a GORM timestamp on this model", kind, name)
				}
				continue
			}
			return fmt.Errorf("admin: %s %q is not a registered field", kind, name)
		}
		return nil
	}
	if err := check("list", opts.List, true); err != nil {
		return err
	}
	if err := check("search", opts.Search, false); err != nil {
		return err
	}
	if err := check("filter", opts.Filter, false); err != nil {
		return err
	}
	if err := check("ordering", opts.Ordering, true); err != nil {
		return err
	}
	return nil
}

func validateQueryableColumns(opts Options, resolved []resolvedField) error {
	byName := make(map[string]*resolvedField, len(resolved))
	for i := range resolved {
		byName[resolved[i].Name] = &resolved[i]
	}
	check := func(kind string, names []string) error {
		for _, name := range names {
			f, ok := byName[name]
			if !ok {
				continue
			}
			if f.column != "" {
				continue
			}
			if f.Type == TypeRelation && f.Related != nil && f.Related.Kind == RelHasMany {
				return fmt.Errorf("admin: %s %q is has_many and has no SQL column", kind, name)
			}
			return fmt.Errorf("admin: %s %q has no SQL column", kind, name)
		}
		return nil
	}
	if err := check("search", opts.Search); err != nil {
		return err
	}
	if err := check("filter", opts.Filter); err != nil {
		return err
	}
	if err := check("ordering", opts.Ordering); err != nil {
		return err
	}
	return nil
}

func resolveFields(fields []Field, sch *schema.Schema) ([]resolvedField, []*m2mBinding, []*relationRead, error) {
	out := make([]resolvedField, 0, len(fields))
	var bindings []*m2mBinding  // many_to_many (read + write)
	var hasMany []*relationRead // has_many (read only)
	seen := map[string]struct{}{}
	for _, f := range fields {
		if _, ok := seen[f.Name]; ok {
			return nil, nil, nil, fmt.Errorf("admin: duplicate field %q", f.Name)
		}
		seen[f.Name] = struct{}{}
		if f.Type == TypeRelation && f.Related != nil && f.Related.Kind == RelHasMany {
			f.ReadOnly = true
		}
		copyRel := f
		if f.Related != nil {
			rel := *f.Related
			copyRel.Related = &rel
		}
		if f.Type == TypeRelation && f.Related != nil && f.Related.Kind == RelManyToMany {
			b, ok := findM2M(sch, f.Name)
			if !ok {
				return nil, nil, nil, fmt.Errorf("admin: many_to_many field %q has no matching association on the model", f.Name)
			}
			binding := b
			bindings = append(bindings, binding)
			out = append(out, resolvedField{
				Field:  copyRel,
				column: "",
				get:    func(inst any) any { return binding.ids(inst) },
				// Write is handled by the join-table sync in create/update.
				set: func(any, any) error { return nil },
			})
			continue
		}
		if f.Type == TypeRelation && f.Related != nil && f.Related.Kind == RelHasMany {
			// has_many is read-only. When it maps to a real GORM has_many
			// association, preload it and expose the related primary keys;
			// otherwise (a meta-only declaration) keep the empty read.
			if binding, ok := findHasMany(sch, f.Name); ok {
				hasMany = append(hasMany, binding)
				out = append(out, resolvedField{
					Field:  copyRel,
					column: "",
					get:    func(inst any) any { return binding.ids(inst) },
					set:    func(any, any) error { return fmt.Errorf("has_many is not writable") },
				})
			} else {
				out = append(out, resolvedField{
					Field:  copyRel,
					column: "",
					get:    func(any) any { return nil },
					set:    func(any, any) error { return fmt.Errorf("has_many is not writable") },
				})
			}
			continue
		}
		sf := matchSchemaField(sch, f)
		if sf == nil {
			return nil, nil, nil, fmt.Errorf("admin: field %q does not exist on the model", f.Name)
		}
		column := f.Column
		if column == "" {
			column = sf.DBName
		}
		out = append(out, resolvedField{
			Field:  copyRel,
			column: column,
			get:    makeGetter(sf.StructField.Index, f.Type),
			set:    makeSetter(sf.StructField.Index, f.Type, sf.FieldType),
		})
	}
	return out, bindings, hasMany, nil
}
