package admin

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/field"
	"github.com/gombit-dev/gombit/resourcepolicy"
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

	derived := len(opts.Fields) == 0
	if derived {
		fields, err := FieldsFrom(model)
		if err != nil {
			return err
		}
		opts.Fields = fields
	}
	// Default Search to the model's text columns when the caller left it unset
	// (nil), so the admin — and the relation pickers, which search server-side —
	// can filter by name out of the box. A caller who wants no search opts out
	// explicitly with an empty (non-nil) slice.
	if err := fillConstraints(opts.Fields, sch); err != nil {
		return err
	}
	if err := alignQuerySurface(&opts, sch, derived); err != nil {
		return err
	}
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
// alignQuerySurface copies filter, ordering, and search from the gombit tag
// when Fields were derived and the caller left those lists unset. A model
// with no gombit tag keeps the previous defaults. An explicit list is kept.
func alignQuerySurface(opts *Options, sch *schema.Schema, derived bool) error {
	if !derived || opts == nil {
		return nil
	}
	explicit := false
	for _, f := range sch.Fields {
		if f != nil && f.Tag.Get("gombit") != "" {
			explicit = true
			break
		}
	}
	if !explicit {
		return nil
	}
	byCol, err := columnPolicy(sch)
	if err != nil {
		return err
	}
	nameOf := make(map[string]string, len(opts.Fields))
	for _, f := range opts.Fields {
		col := f.Column
		if col == "" {
			col = f.Name
		}
		nameOf[col] = f.Name
	}
	if opts.Filter == nil {
		if names := policyNames(byCol, nameOf, func(r resourcepolicy.Resolved) bool { return r.Filterable }); len(names) > 0 {
			opts.Filter = names
		}
	}
	if opts.Ordering == nil {
		if names := policyNames(byCol, nameOf, func(r resourcepolicy.Resolved) bool { return r.Sortable }); len(names) > 0 {
			opts.Ordering = names
		}
	}
	// Search defaults to every text column. Once a model declares policy,
	// that default would search columns the public API does not, including
	// a write-only secret. An empty slice opts out.
	if opts.Search == nil {
		opts.Search = policyNames(byCol, nameOf, func(r resourcepolicy.Resolved) bool { return r.Searchable })
	}
	return nil
}

func policyNames(byCol map[string]resourcepolicy.Resolved, nameOf map[string]string, keep func(resourcepolicy.Resolved) bool) []string {
	out := []string{}
	for col, r := range byCol {
		if !keep(r) {
			continue
		}
		name, ok := nameOf[col]
		if !ok {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func defaultSearchFields(fields []Field) []string {
	var out []string
	for _, f := range fields {
		if f.ReadOnly || f.WriteOnly {
			continue
		}
		if f.Type == TypeText || (f.Type == TypeString && field.AllowsSearch(f.queryKind(), "")) {
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

// queryKind is the catalog kind for list policy. Email, URL, IP, and slug
// share the string admin wire, so the format or slug pattern recovers the
// kind. A plain string stays searchable and filterable.
func (f Field) queryKind() field.Kind {
	if k, ok := field.SemanticKind(f.Format, f.TagPattern, f.Pattern); ok {
		return k
	}
	return field.Kind(f.Type)
}

// fieldAllowsQuery reports whether an admin list option (search, filter,
// ordering) is legal for the field's catalog kind. Relation cardinalities
// read relationCaps. Semantic strings use the kind recovered from format
// or the slug pattern, not the string wire type.
func fieldAllowsQuery(kind string, f Field) bool {
	k := f.queryKind()
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
			if f.Related.Kind != RelBelongsTo && f.Related.Kind != RelHasMany && f.Related.Kind != RelManyToMany && f.Related.Kind != RelOneToOne {
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
					detail := string(f.queryKind())
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

// fillConstraints copies an empty constraint from the model's validate tag.
// A value the registrar already set is left alone.
func fillConstraints(fields []Field, sch *schema.Schema) error {
	for i := range fields {
		f := &fields[i]
		if f.Type == TypeRelation {
			continue
		}
		sf := matchSchemaField(sch, *f)
		if sf == nil {
			continue
		}
		c, err := field.ParseConstraints(sf.Tag.Get("validate"))
		if err != nil {
			return fmt.Errorf("admin: field %q: %w", f.Name, err)
		}
		if f.Minimum == "" {
			f.Minimum = c.Min
		}
		if f.Maximum == "" {
			f.Maximum = c.Max
		}
		if f.MaxLength == 0 {
			f.MaxLength = c.MaxLength
		}
		if f.TagPattern == "" {
			f.TagPattern = sf.Tag.Get("pattern")
		}
		if f.Pattern == "" {
			f.Pattern = c.Pattern
		}
		if f.Pattern == "" {
			f.Pattern = f.TagPattern
		}
		if f.Format == "" {
			f.Format = sf.Tag.Get("format")
		}
		if f.Default == "" {
			f.Default = c.Default
		}
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
			Field:   copyRel,
			column:  column,
			pointer: sf.FieldType.Kind() == reflect.Pointer,
			get:     makeGetter(sf.StructField.Index, f.Type),
			set:     makeSetter(sf.StructField.Index, f.Type, sf.FieldType),
		})
	}
	return out, bindings, hasMany, nil
}
