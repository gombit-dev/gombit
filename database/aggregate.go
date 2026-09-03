package database

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/types"
	"gorm.io/gorm"
)

// This file holds the reusable server-side aggregate primitives (SUM / AVG /
// MIN / MAX over a declared numeric column) that the generated resource list
// handler calls. Like the list-query helpers in listquery.go, the generator
// only emits calls for fields a resource declares aggregatable, so the surface
// stays a safe, declared subset. Aggregates run over the same filtered/searched
// query the list uses, computed before pagination — so a "total invoiced this
// month" card reflects the whole matching set, not one page (issue #272).

// AggregateFunc is a supported SQL aggregate function.
type AggregateFunc string

const (
	AggSum AggregateFunc = "sum"
	AggAvg AggregateFunc = "avg"
	AggMin AggregateFunc = "min"
	AggMax AggregateFunc = "max"
)

// aggregateSQL maps a validated func to its SQL keyword. Only these four are
// reachable — ParseAggregates rejects anything else before it becomes a spec —
// so the keyword is never derived from user input.
var aggregateSQL = map[AggregateFunc]string{
	AggSum: "SUM",
	AggAvg: "AVG",
	AggMin: "MIN",
	AggMax: "MAX",
}

// AggregateColumn declares one field a resource exposes to aggregates: the DB
// column the SQL function is applied to. It is generator-controlled — built
// from a field declared `aggregatable` — so only a declared numeric column can
// reach the query.
type AggregateColumn struct {
	Column string
}

// AggregateSpec is one resolved aggregate request: Func over Column, returned in
// the response keyed as "<func>:<field>" (Key).
type AggregateSpec struct {
	Func   AggregateFunc
	Column string
	Key    string
}

// ParseAggregates turns the raw `aggregate` query param — a comma-separated list
// of "<func>:<field>" pairs, e.g. "sum:total,avg:total" — into resolved specs.
//
//   - An empty (or whitespace-only) raw value returns nil (no aggregates asked
//     for), so a plain list request is unchanged.
//   - func is matched case-insensitively against sum / avg / min / max.
//   - field must be a key in allowed (a declared aggregatable field); the value
//     supplies the DB column. allowed is generator-controlled; only raw is user
//     input, so an undeclared field cannot reach the query.
//   - A malformed pair, unknown func, or undeclared field returns a D10
//     validation error (422) keyed on "aggregate".
//   - Duplicate pairs collapse to one spec (same key), so the response map has
//     one entry per requested aggregate.
func ParseAggregates(ctx context.Context, raw string, allowed map[string]AggregateColumn) ([]AggregateSpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	invalid := func() error {
		return contract.WithContext(ctx, contract.Validation(
			"The request contains invalid fields.",
			map[string][]string{"aggregate": {"must be a comma-separated list of <func>:<field>, e.g. sum:total,avg:total"}},
		))
	}
	specs := make([]AggregateSpec, 0)
	seen := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fnRaw, field, ok := strings.Cut(part, ":")
		if !ok {
			return nil, invalid()
		}
		fn := AggregateFunc(strings.ToLower(strings.TrimSpace(fnRaw)))
		if _, ok := aggregateSQL[fn]; !ok {
			return nil, invalid()
		}
		field = strings.TrimSpace(field)
		col, ok := allowed[field]
		if !ok {
			return nil, invalid()
		}
		key := string(fn) + ":" + field
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		specs = append(specs, AggregateSpec{Func: fn, Column: col.Column, Key: key})
	}
	if len(specs) == 0 {
		return nil, nil
	}
	return specs, nil
}

// Aggregate computes the requested aggregates over q — which already carries the
// list's filters and search — and returns a map keyed by each spec's Key
// ("<func>:<field>"). All aggregates are evaluated in a single SELECT with no
// GROUP BY, so the query returns exactly one row and runs before pagination.
//
// Values are the database's own aggregate, encoded as a types.Decimal (a
// canonical JSON string). Precision therefore follows the driver: Postgres and
// MySQL compute SUM/AVG/MIN/MAX over numeric/decimal in fixed point, so a
// decimal SUM (and integer aggregates on every driver) is exact; SQLite has no
// native fixed-point aggregate and computes AVG — and SUM of a fractional
// decimal column — in IEEE double, so those may carry float rounding and can
// differ from Postgres/MySQL for the same data. AVG is fractional by nature
// (rounded to each driver's default scale) and is an approximation everywhere.
// See docs/contract.md "List query: numeric aggregates" for the per-driver
// contract. A NULL result — an aggregate over an empty matching set (SUM/AVG/
// MIN/MAX all yield NULL) — becomes decimal zero, so a card renders 0 not null.
//
// An empty specs list is a no-op (nil map, no query). Column names in specs are
// generator-controlled; nothing here is derived from user input.
func Aggregate(ctx context.Context, q *gorm.DB, specs []AggregateSpec) (map[string]types.Decimal, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	selects := make([]string, len(specs))
	aliasKey := make(map[string]string, len(specs))
	for i, s := range specs {
		alias := fmt.Sprintf("agg%d", i)
		selects[i] = aggregateSQL[s.Func] + "(" + quoteIdent(q, s.Column) + ") AS " + alias
		aliasKey[alias] = s.Key
	}
	// A no-GROUP-BY aggregate SELECT returns exactly one row. Find into a slice
	// of maps (not Scan into a single map, which returns *interface{} pointers
	// per column rather than the scanned values) yields driver-typed scalars.
	var scanned []map[string]any
	if err := q.WithContext(ctx).Select(strings.Join(selects, ", ")).Find(&scanned).Error; err != nil {
		return nil, contract.WithContext(ctx, contract.Internal("aggregate resources"))
	}
	var row map[string]any
	if len(scanned) > 0 {
		row = scanned[0]
	}
	out := make(map[string]types.Decimal, len(specs))
	for alias, key := range aliasKey {
		dec, err := toDecimal(row[alias])
		if err != nil {
			return nil, contract.WithContext(ctx, contract.Internal("aggregate resources"))
		}
		out[key] = dec
	}
	return out, nil
}

// toDecimal normalizes a driver-scanned aggregate value into a types.Decimal.
// Drivers return aggregate results with different Go types: Postgres yields int64
// for SUM(int) and a numeric string/[]byte for fixed-point AVG/decimal SUM,
// MySQL yields []byte for its decimal results — both exact. SQLite yields int64
// for integer aggregates but float64 for AVG and for SUM of a fractional decimal
// column, because it has no fixed-point aggregate; that float64 is encoded via
// its shortest round-tripping decimal string, so the value is faithful to what
// SQLite computed (float rounding included) rather than silently reshaped. A
// NULL result (nil) becomes decimal zero.
func toDecimal(v any) (types.Decimal, error) {
	switch val := v.(type) {
	case nil:
		return types.Decimal{}, nil
	case []byte:
		return types.NewDecimalFromString(strings.TrimSpace(string(val)))
	case string:
		return types.NewDecimalFromString(strings.TrimSpace(val))
	case int64:
		return types.NewDecimalFromString(strconv.FormatInt(val, 10))
	case int32:
		return types.NewDecimalFromString(strconv.FormatInt(int64(val), 10))
	case float64:
		return types.NewDecimalFromString(strconv.FormatFloat(val, 'f', -1, 64))
	case float32:
		return types.NewDecimalFromString(strconv.FormatFloat(float64(val), 'f', -1, 32))
	default:
		return types.NewDecimalFromString(fmt.Sprintf("%v", val))
	}
}
