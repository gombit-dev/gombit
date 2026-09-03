package database

import (
	"context"
	"strconv"
	"strings"

	"github.com/gombit-dev/gombit/contract"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// FilterKind is the column type an exact-match filter coerces its raw query
// string to before comparing. Filters arrive as strings (Huma does not support
// optional/pointer query params, so an empty string is the "absent" signal —
// the same convention the admin data plane uses), and the kind decides how the
// string is parsed and bound.
type FilterKind int

const (
	FilterString FilterKind = iota
	FilterInt
	FilterInt64
	FilterUint
	FilterBool
)

// This file holds the reusable list-query primitives (exact-match filter,
// text search, validated sort) that the generated resource list handler calls.
// The generator only emits calls for fields a resource declares filterable /
// searchable / sortable, so the query surface stays a safe, declared subset —
// the same contract the admin data plane applies (admin/resources.go), extracted
// here so both the framework-owned admin and generated apps share one behavior.

// FilterEq adds an exact-match `column = <raw coerced to kind>` predicate. An
// empty (or whitespace-only) raw value is a no-op — the filter was not supplied
// — so callers can chain one FilterEq per declared filterable field. A raw value
// that does not parse as kind returns a D10 validation error (422) keyed on
// column. The column name is generator-controlled (a declared field's DB
// column); only raw is user input.
func FilterEq(ctx context.Context, q *gorm.DB, column string, kind FilterKind, raw string) (*gorm.DB, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return q, nil
	}
	var (
		value any
		err   error
	)
	switch kind {
	case FilterInt:
		value, err = strconv.Atoi(raw)
	case FilterInt64:
		value, err = strconv.ParseInt(raw, 10, 64)
	case FilterUint:
		var u uint64
		u, err = strconv.ParseUint(raw, 10, 64)
		value = uint(u)
	case FilterBool:
		value, err = strconv.ParseBool(raw)
	default: // FilterString
		value = raw
	}
	if err != nil {
		return nil, contract.WithContext(ctx, contract.Validation(
			"The request contains invalid fields.",
			map[string][]string{column: {"invalid filter value"}},
		))
	}
	return q.Where(clause.Eq{Column: clause.Column{Name: column}, Value: value}), nil
}

// Search adds a case-insensitive OR of `LOWER(col) LIKE LOWER(%term%)` across
// the given columns. An empty (or whitespace-only) term, or an empty column
// list, is a no-op. The term is treated as a literal: LIKE wildcards in it are
// escaped, so a user searching for "50%" matches the literal text, not a
// prefix. Columns are generator-controlled; only the term is user input.
func Search(q *gorm.DB, columns []string, term string) *gorm.DB {
	term = strings.TrimSpace(term)
	if term == "" || len(columns) == 0 {
		return q
	}
	pattern := "%" + escapeLike(term) + "%"
	ors := make([]clause.Expression, 0, len(columns))
	for _, col := range columns {
		ors = append(ors, clause.Expr{
			SQL:  "LOWER(" + quoteIdent(q, col) + ") LIKE LOWER(?) ESCAPE ?",
			Vars: []any{pattern, `\`},
		})
	}
	return q.Where(clause.Or(ors...))
}

// SortBy applies `ORDER BY sort [ASC|DESC]`, validated against the allowed set.
//
//   - sort empty: order by fallback (the stable default, e.g. "id") when
//     fallback is non-empty; otherwise no ORDER BY is added.
//   - sort present but not in allowed: a 422 validation error on "sort".
//   - order is "" or "asc" (ascending) or "desc"; anything else is a 422 on
//     "order".
//
// allowed and fallback are generator-controlled (a resource's declared sortable
// columns); sort and order are user input, matched by exact string equality so
// only a declared column can reach the query.
func SortBy(ctx context.Context, q *gorm.DB, sort, order string, allowed []string, fallback string) (*gorm.DB, error) {
	sort = strings.TrimSpace(sort)
	if sort == "" {
		if fallback != "" {
			q = q.Order(clause.OrderByColumn{Column: clause.Column{Name: fallback}})
		}
		return q, nil
	}
	permitted := false
	for _, a := range allowed {
		if a == sort {
			permitted = true
			break
		}
	}
	if !permitted {
		return nil, contract.WithContext(ctx, contract.Validation(
			"The request contains invalid fields.",
			map[string][]string{"sort": {"sorting is not allowed for this field"}},
		))
	}
	var desc bool
	switch strings.ToLower(strings.TrimSpace(order)) {
	case "", "asc":
		desc = false
	case "desc":
		desc = true
	default:
		return nil, contract.WithContext(ctx, contract.Validation(
			"The request contains invalid fields.",
			map[string][]string{"order": {"must be asc or desc"}},
		))
	}
	return q.Order(clause.OrderByColumn{Column: clause.Column{Name: sort}, Desc: desc}), nil
}

// escapeLike escapes the LIKE metacharacters so a search term matches literally
// under the `ESCAPE '\'` clause Search emits.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// quoteIdent quotes a column identifier using the active dialect's quoting rules.
func quoteIdent(db *gorm.DB, name string) string {
	var b strings.Builder
	db.QuoteTo(&b, name)
	return b.String()
}
