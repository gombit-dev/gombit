// Package resourcepolicy is the foundational slice of the model-first resource
// generator (ADR-016, issue #352). It defines the `gombit` field-policy tag and
// resolves, per model field, the API behavior the generator derives DTOs,
// mappers, and CRUD plumbing from.
//
// Ownership of facts is deliberately split (ADR-016): the GORM schema owns
// persistence facts (DB-backed / primary key / auto-managed / column name /
// nullability / default) and policy MUST NOT restate them. The `gombit` tag owns
// only API behavior:
//
//	read     the field appears in the response DTO
//	write    the field is settable via the create request DTO (create source = request)
//	server   the field is set server-side (a hook), not from the request (create source = server)
//	-        the field is hidden from the API entirely (no request, no response)
//
// A field with no `gombit` tag takes a default by kind: a regular content column
// is read+write from the request; the primary key and auto timestamps are
// read-only; soft-delete is hidden. Persistence facts are supplied by the caller
// (slice 2 reads them from the model); this slice is pure and side-effect free.
package resourcepolicy

import (
	"fmt"
	"sort"
	"strings"
)

// FieldFacts are the persistence facts about a model field, owned by the GORM
// schema. Policy never restates these; it only layers API behavior on top.
type FieldFacts struct {
	GoName     string // Go field name, e.g. "Title"
	Column     string // DB column name, e.g. "title"; empty for a relationship/association field
	DBBacked   bool   // maps to a database column (false for has_many/many2many/belongs-to object fields)
	PrimaryKey bool
	AutoTime   bool // auto create/update timestamp (CreatedAt/UpdatedAt or autoCreateTime/autoUpdateTime)
	SoftDelete bool // gorm.DeletedAt
}

// autoManaged reports whether the database or GORM fills the column itself, so it
// is never settable through the create request.
func (f FieldFacts) autoManaged() bool {
	return f.PrimaryKey || f.AutoTime || f.SoftDelete
}

// CreateSource is where a persisted field's create value comes from.
type CreateSource int

const (
	// CreateSourceNone is for fields the create path does not set (auto-managed,
	// hidden, or not DB-backed).
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
	if p.hidden && (p.read || p.write || p.server) {
		return p, fmt.Errorf("resourcepolicy: gombit:\"-\" (hidden) cannot be combined with read/write/server")
	}
	if p.write && p.server {
		return p, fmt.Errorf("resourcepolicy: gombit tag has both write and server; a field is set from the request OR server-side, not both")
	}
	return p, nil
}

// Resolve combines a field's persistence facts with its `gombit` tag into its API
// behavior. It returns an error for a contradictory tag (e.g. writing an
// auto-managed column, or write+server together).
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

	if tag.hidden {
		return r, nil // no request, no response, no create source
	}

	if facts.autoManaged() {
		if tag.write || tag.server {
			return Resolved{}, fmt.Errorf("resourcepolicy: column %q is auto-managed (primary key / timestamp / soft-delete) and cannot be written or server-set", facts.Column)
		}
		if tag.present {
			r.InResponse = tag.read
		} else {
			// Default: PK and timestamps are readable; soft-delete is hidden.
			r.InResponse = !facts.SoftDelete
		}
		return r, nil // auto-managed columns are never a create source
	}

	// Regular content column.
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
		// Default: a content column is readable and writable from the request.
		r.InResponse = true
		r.InRequest = true
		r.CreateSource = CreateSourceRequest
	}
	return r, nil
}

// ResolveAll resolves every field, returning results in the input order. It fails
// on the first contradictory field so the generator never emits from a bad policy.
func ResolveAll(fields []FieldFacts, tags map[string]string) ([]Resolved, error) {
	out := make([]Resolved, 0, len(fields))
	for _, f := range fields {
		r, err := Resolve(f, tags[f.GoName])
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
