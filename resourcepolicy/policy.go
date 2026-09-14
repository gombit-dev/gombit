// Package resourcepolicy is the foundational slice of the model-first resource
// generator (ADR-016, issue #352). It defines the `gombit` field-policy tag and
// resolves, per model field, the API behavior the generator derives DTOs,
// mappers, and CRUD plumbing from.
//
// Ownership of facts is deliberately split (ADR-016): the GORM schema owns
// persistence facts (DB-backed / primary key / auto-increment / DB default /
// nullability / auto timestamp / soft-delete / column name) and policy MUST NOT
// restate them. This resolver carries those facts losslessly — it never collapses
// them — because create-source correctness depends on the exact combination (a
// manual string key is not auto-generated; a NOT NULL column with no default must
// be supplied on create). The `gombit` tag owns only API behavior:
//
//	read     the field appears in the response DTO
//	write    the field is settable via the create request DTO (create source = request)
//	server   the field is set server-side (a hook), not from the request (create source = server)
//	-        the field is hidden from the API (no request, no response)
//
// `-` may combine with `server` (hidden from the API but set by a hook); it may
// not combine with `read`/`write`, and `write`+`server` conflict. A field with no
// `gombit` tag defaults by kind: a regular content column and a manual primary
// key are read+write from the request; an auto-increment key and auto timestamps
// are read-only; soft-delete is hidden. Resolve rejects a required column (NOT
// NULL / primary key, no `default:` clause, not DB-generated) that ends up with
// no create source — the silent zero-fill #352 exists to eliminate. It does not
// evaluate whether a default's SQL yields a non-null value; the database enforces
// that and fails loudly, so a `default:` clause defers to the database.
//
// Persistence facts are supplied by the caller (slice 2 reads them from the
// model); this slice is pure and side-effect free.
package resourcepolicy

import (
	"fmt"
	"sort"
	"strings"
)

// FieldFacts are the persistence facts about a model field, owned by the GORM
// schema. Policy never restates these; it only layers API behavior on top. The
// facts are kept independent (not collapsed) because create-source correctness
// depends on their exact combination.
type FieldFacts struct {
	GoName        string // Go field name, e.g. "Title"
	Column        string // DB column name, e.g. "title"; empty for a relationship/association field
	DBBacked      bool   // maps to a database column (false for has_many/many2many/belongs-to object fields)
	PrimaryKey    bool
	AutoIncrement bool // database-generated key (serial/identity)
	AutoTime      bool // auto create/update timestamp (autoCreateTime/autoUpdateTime)
	SoftDelete    bool // gorm.DeletedAt
	NotNull       bool // NOT NULL constraint
	HasDefault    bool // a `default:` clause is present. Whether that default's SQL expression actually yields a non-null value is the database's concern (it enforces NOT NULL and rejects a bad default loudly at migrate/insert), NOT something this pure layer evaluates.

	// Creatable / Readable mirror GORM's per-field permissions (schema.Field
	// Creatable/Readable, driven by the `->`/`<-` permission tags), independent of
	// whether the field has a column. GORM OMITS non-creatable fields from INSERT
	// and skips non-readable fields on SELECT, so policy must honor them or the API
	// promises an operation persistence will not perform. Normal columns are both
	// true; a `gorm:"->"` read-only/computed column is Creatable=false, and a
	// `gorm:"->:false;<-:create"` column is Readable=false.
	Creatable bool
	Readable  bool
}

// managed reports whether GORM owns the column's lifecycle, so it is never
// user-settable through the create request or a hook. This answers only "may the
// API write it?" — NOT "does persistence supply a valid value on create" (see
// dbSuppliesCreateValue). A manual string/UUID primary key is not managed.
func (f FieldFacts) managed() bool {
	return f.AutoIncrement || f.AutoTime || f.SoftDelete
}

// dbSuppliesCreateValue reports whether the DB or GORM writes a valid value for
// the column during INSERT. This is narrower than managed(): an auto-increment
// key is DB-generated; an auto timestamp supplies a value ONLY when GORM writes
// the field (i.e. it is Creatable — `<-:update` auto-time is not); and
// soft-delete is managed on DELETE, not create, so it never supplies a create
// value.
func (f FieldFacts) dbSuppliesCreateValue() bool {
	return f.AutoIncrement || (f.AutoTime && f.Creatable)
}

// requiresCreateValue reports whether an omitted column would be SILENTLY
// zero-filled at create — the #218 drift this layer guards. That happens when a
// NOT NULL (or primary-key) column has no `default:` clause, is not supplied by
// the DB/GORM, and nothing in the API sources it: GORM then sends the Go zero
// value, which passes NOT NULL as wrong data.
//
// This deliberately keys on the PRESENCE of a default, not its value. Whether a
// default expression yields a value satisfying NOT NULL is dialect-specific SQL
// this pure layer cannot evaluate (`default:null`, `default:(NULL)`,
// `default:(coalesce(NULL,NULL))` …); the database owns that and rejects a bad
// default LOUDLY at migrate/insert. Loud DB errors are not the silent drift this
// guard exists to prevent, so a `default:` clause defers to the database.
//
// requiresCreateValue is INDEPENDENT of Creatable — Creatable governs whether
// GORM writes a value the API provides, not whether one exists. A required column
// that neither the DB supplies nor the API can source is unsatisfiable; Resolve
// reports how.
func (f FieldFacts) requiresCreateValue() bool {
	if !f.DBBacked {
		return false
	}
	return (f.NotNull || f.PrimaryKey) && !f.HasDefault && !f.dbSuppliesCreateValue()
}

// CreateSource is where a persisted field's create value comes from.
type CreateSource int

const (
	// CreateSourceNone is for fields the create path does not set (auto-generated,
	// or a nullable/defaulted column left unset, or not DB-backed).
	CreateSourceNone CreateSource = iota
	// CreateSourceRequest: the value comes from the create request body.
	CreateSourceRequest
	// CreateSourceServer: the value is set server-side (a BeforeCreate hook).
	CreateSourceServer
)

func (s CreateSource) String() string {
	switch s {
	case CreateSourceRequest:
		return "request"
	case CreateSourceServer:
		return "server"
	default:
		return "none"
	}
}

// Resolved is a field's resolved API behavior: its persistence facts plus where
// it appears in the API and where its create value comes from.
type Resolved struct {
	FieldFacts
	InRequest    bool         // settable via the create request DTO
	InResponse   bool         // surfaced in the response DTO
	CreateSource CreateSource // where a persisted create value comes from
}

// Field pairs a field's persistence facts with its raw `gombit` tag. It keeps the
// association 1:1: GORM can flatten two embedded fields to the same GoName with
// different columns and tags, so policy is never keyed by name.
type Field struct {
	FieldFacts
	Tag string
}

// policyTag is the parsed intent of a `gombit:"..."` tag.
type policyTag struct {
	present bool
	read    bool
	write   bool
	server  bool
	hidden  bool
}

// parsePolicyTag parses the comma-separated `gombit` tag value. An empty tag
// (field carried no `gombit:"..."`) yields present=false, so kind defaults apply.
func parsePolicyTag(tag string) (policyTag, error) {
	p := policyTag{}
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return p, nil
	}
	p.present = true
	for _, tok := range strings.Split(tag, ",") {
		switch strings.TrimSpace(tok) {
		case "read":
			p.read = true
		case "write":
			p.write = true
		case "server":
			p.server = true
		case "-":
			p.hidden = true
		case "":
			// tolerate a stray empty token from a trailing comma
		default:
			return p, fmt.Errorf("resourcepolicy: unknown gombit policy token %q (want read, write, server, or -)", strings.TrimSpace(tok))
		}
	}
	if p.hidden && (p.read || p.write) {
		return p, fmt.Errorf("resourcepolicy: gombit:\"-\" (hidden) cannot be combined with read/write (it may combine with server)")
	}
	if p.write && p.server {
		return p, fmt.Errorf("resourcepolicy: gombit tag has both write and server; a field is set from the request OR server-side, not both")
	}
	return p, nil
}

// Resolve combines a field's persistence facts with its `gombit` tag into its API
// behavior. It errors on a contradictory tag (writing/server-setting a managed or
// non-creatable column, reading a non-readable one, write+server, tagging a
// relationship) and on a required column that no create source can satisfy —
// whether because the policy gives it none or because GORM never writes a valid
// value for it (a non-creatable, `<-:update` auto-time, or NOT NULL soft-delete
// column). The two questions "may the API write this?" (managed / capability) and
// "does a valid value reach the INSERT?" (dbSuppliesCreateValue) are kept apart.
func Resolve(facts FieldFacts, gombitTag string) (Resolved, error) {
	tag, err := parsePolicyTag(gombitTag)
	if err != nil {
		return Resolved{}, err
	}
	r := Resolved{FieldFacts: facts, CreateSource: CreateSourceNone}

	// A relationship/association field is not a scalar wire column; it is not part
	// of the derived request/response DTOs (belongs-to exposes its FK column, which
	// is itself DB-backed and handled as a normal field).
	if !facts.DBBacked {
		if tag.present {
			return Resolved{}, fmt.Errorf("resourcepolicy: field %q is not a database column (relationship); it cannot carry a gombit API policy", facts.GoName)
		}
		return r, nil
	}

	managed := facts.managed() // GORM-owned lifecycle: never user-settable

	// Validate the tag against what the API and persistence can actually do.
	if tag.write || tag.server {
		if managed {
			return Resolved{}, fmt.Errorf("resourcepolicy: column %q is managed by GORM (auto-increment / timestamp / soft-delete) and cannot be written or server-set", facts.Column)
		}
		if !facts.Creatable {
			return Resolved{}, fmt.Errorf("resourcepolicy: column %q is not creatable (GORM omits it from INSERT) and cannot be written or server-set", facts.Column)
		}
	}
	if tag.read && !facts.Readable {
		return Resolved{}, fmt.Errorf("resourcepolicy: column %q is not readable (GORM skips it on SELECT) and cannot be in the response", facts.Column)
	}

	// Surfaces + create source. Defaults respect capabilities: readable and not
	// soft-delete → response; creatable and not GORM-managed → request.
	if tag.present {
		r.InResponse = tag.read
		r.InRequest = tag.write
		switch {
		case tag.write:
			r.CreateSource = CreateSourceRequest
		case tag.server:
			r.CreateSource = CreateSourceServer
		}
	} else {
		r.InResponse = facts.Readable && !facts.SoftDelete
		r.InRequest = facts.Creatable && !managed
		if r.InRequest {
			r.CreateSource = CreateSourceRequest
		}
	}

	// A required column must receive a valid value at create time (from the DB,
	// or from the API). requiresCreateValue already excludes DB-supplied values,
	// so if it still needs one and has no source, the model is broken — report why.
	if facts.requiresCreateValue() && r.CreateSource == CreateSourceNone {
		switch {
		case !facts.Creatable:
			return Resolved{}, fmt.Errorf("resourcepolicy: required column %q is not creatable (GORM omits it from INSERT) yet has no default or auto-generation, so nothing can supply it on create: make it nullable, give it a database default, or drop the read-only/`<-:update` permission so it can be created", facts.Column)
		case managed:
			return Resolved{}, fmt.Errorf("resourcepolicy: column %q is GORM-managed (timestamp / soft-delete) but NOT NULL with no default, and GORM writes no valid value for it on create: make it nullable or give it a database default", facts.Column)
		default:
			return Resolved{}, fmt.Errorf("resourcepolicy: required column %q has no create source: add write (from the request), server (a hook), a database default, or make it nullable", facts.Column)
		}
	}
	return r, nil
}

// ResolveAll resolves every field, in input order, preserving each field's own
// facts+tag pairing (never keyed by the possibly-ambiguous Go name). It fails on
// the first contradictory field so the generator never emits from a bad policy.
func ResolveAll(fields []Field) ([]Resolved, error) {
	out := make([]Resolved, 0, len(fields))
	for _, f := range fields {
		r, err := Resolve(f.FieldFacts, f.Tag)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// RequestColumns returns the DB column names settable via the create request,
// sorted — the write surface the generated request DTO must expose.
func RequestColumns(resolved []Resolved) []string {
	return columnsWhere(resolved, func(r Resolved) bool { return r.InRequest })
}

// ResponseColumns returns the DB column names surfaced in responses, sorted.
func ResponseColumns(resolved []Resolved) []string {
	return columnsWhere(resolved, func(r Resolved) bool { return r.InResponse })
}

// ServerColumns returns the DB column names whose create value is set server-side
// (a hook), sorted — the create-time non-request source.
func ServerColumns(resolved []Resolved) []string {
	return columnsWhere(resolved, func(r Resolved) bool { return r.CreateSource == CreateSourceServer })
}

func columnsWhere(resolved []Resolved, keep func(Resolved) bool) []string {
	var out []string
	for _, r := range resolved {
		if r.Column != "" && keep(r) {
			out = append(out, r.Column)
		}
	}
	sort.Strings(out)
	return out
}
