package admin

import "github.com/gombit-dev/gombit/field"

// Admin meta type strings. These are the admin projection of field.Kind
// (see docs/fields.md). Do not add a wire string that package field does
// not already list.
const (
	TypeString   FieldType = FieldType(field.String)
	TypeText     FieldType = FieldType(field.Text)
	TypeInteger  FieldType = FieldType(field.Integer)
	TypeFloat    FieldType = FieldType(field.Float)
	TypeDecimal  FieldType = FieldType(field.Decimal)
	TypeBoolean  FieldType = FieldType(field.Boolean)
	TypeDateTime FieldType = FieldType(field.DateTime)
	TypeDate     FieldType = FieldType(field.Date)
	TypeUUID     FieldType = FieldType(field.UUID)
	TypeJSON     FieldType = FieldType(field.JSON)
	TypeRelation FieldType = FieldType(field.Relation)
)

// Relation kinds for v1. Defined in package field.
const (
	RelBelongsTo  = string(field.RelBelongsTo)
	RelHasMany    = string(field.RelHasMany)
	RelManyToMany = string(field.RelManyToMany)
)

// Implicit timestamp names allowed in List and Ordering even when omitted
// from Fields. They are GORM's default created_at / updated_at columns.
const (
	ImplicitCreatedAt = "created_at"
	ImplicitUpdatedAt = "updated_at"
)

// FieldType is a closed admin field type string.
type FieldType string

// Options is the source of truth for one registered admin model.
type Options struct {
	// Slug is the URL key (products). Required, lowercase, unique per app.
	Slug string
	// Singular and Plural are UI labels. Empty values are derived at Register
	// from the Go type name and slug.
	Singular string
	Plural   string
	// PK is the JSON/field name of the primary key. Empty means derive the
	// GORM primary key at Register and store it.
	PK string
	// Fields is the concrete field list handlers read. Empty means derive a
	// default from the struct once, inside Register (see FieldsFrom).
	Fields []Field
	// List is the list-view column order.
	List []string
	// Search is the list of field names the search query param applies to.
	Search []string
	// Filter is the list of field names that may appear as list query keys.
	Filter []string
	// Ordering is the list of field names the ordering query param may use.
	// created_at and updated_at may appear here even if omitted from Fields.
	Ordering []string
	// Actions enables list / detail / create / update / delete. The zero
	// value (all false) defaults to all enabled.
	Actions Actions
	// Permissions are authorization keys enforced by the admin handlers.
	// Empty values default to admin.{slug}.{action}.
	Permissions Permissions
}

// Field describes one registered admin field.
//
// Name is the JSON object key used in meta and in data-plane row payloads.
// For v1, Name is also the GORM/SQL column unless Column is set (when the
// Go exported name or GORM column differs from the JSON key).
type Field struct {
	Name     string    `json:"name"`
	Type     FieldType `json:"type"`
	Required bool      `json:"required"`
	ReadOnly bool      `json:"readonly"`
	Related  *Relation `json:"related,omitempty"`
	// Constraints copied from the model's validate tag when the registrar
	// leaves them empty. Strings so a decimal bound survives JSON.
	Minimum   string `json:"minimum,omitempty"`
	Maximum   string `json:"maximum,omitempty"`
	MaxLength int    `json:"max_length,omitempty"`
	Pattern   string `json:"pattern,omitempty"`
	Default   string `json:"default,omitempty"`
	// Format is the OpenAPI format from the model tag (email, uri, ip).
	// The admin wire stays string; writes reject a value that fails it.
	Format string `json:"format,omitempty"`
	// Column is the GORM/SQL column name. Empty means Name == JSON key ==
	// column (the v1 default). Not emitted in meta.
	Column string `json:"-"`
}

// Relation describes a belongs_to, has_many, or many_to_many field (#223).
//
//   - belongs_to is stored as the foreign key on create/update.
//   - has_many is read-only. When the field maps to a real GORM has_many
//     association (the usual case, and what auto-derivation emits), list/detail
//     preload it and the data plane returns the related children's primary keys;
//     a has_many field declared without a matching association is meta-only and
//     reads empty. Writes to a has_many field are always rejected.
//   - many_to_many reads the related primary keys and, on write, syncs the join
//     table to the submitted id list.
type Relation struct {
	Slug       string `json:"slug"`
	Kind       string `json:"kind"`
	LabelField string `json:"label_field"`
}

// Actions names which data-plane operations are enabled for a model.
type Actions struct {
	List   bool `json:"list"`
	Detail bool `json:"detail"`
	Create bool `json:"create"`
	Update bool `json:"update"`
	Delete bool `json:"delete"`
}

// Permissions holds the keys enforced for each admin operation.
type Permissions struct {
	View   string `json:"view"`
	Create string `json:"create"`
	Update string `json:"update"`
	Delete string `json:"delete"`
}

func (a Actions) zero() bool {
	return !a.List && !a.Detail && !a.Create && !a.Update && !a.Delete
}

func defaultActions() Actions {
	return Actions{List: true, Detail: true, Create: true, Update: true, Delete: true}
}

func validFieldType(t FieldType) bool {
	return field.IsAdminWire(string(t))
}

func implicitTimestamp(name string) bool {
	return name == ImplicitCreatedAt || name == ImplicitUpdatedAt
}
