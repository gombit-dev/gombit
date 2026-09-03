package database

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/types"
	"gorm.io/gorm"
)

type aggItem struct {
	ID       uint `gorm:"primaryKey"`
	AuthorID uint
	Amount   int
}

// aggMoney carries a fixed-point decimal column so the precision contract can be
// exercised: a fractional decimal SUM and a non-integer AVG, the two cases the
// integer-only aggItem cannot reach.
type aggMoney struct {
	ID     uint          `gorm:"primaryKey"`
	Amount types.Decimal `gorm:"type:decimal(19,4)"`
}

// TestAggregateHelpersSQLite exercises ParseAggregates / Aggregate on SQLite.
// The same assertions run against Postgres and MySQL from the integration suite
// (TestAggregateHelpers{Postgres,MySQL}), so the SQLite+PG+MySQL matrix the
// working agreement requires is covered.
func TestAggregateHelpersSQLite(t *testing.T) {
	assertAggregateHelpers(t, openSQLite(t))
}

func assertAggregateHelpers(t *testing.T, db *DB) {
	t.Helper()
	seedAggregate(t, db)
	ctx := context.Background()
	allowed := map[string]AggregateColumn{"amount": {Column: "amount"}}

	t.Run("EmptyRawIsNoOp", func(t *testing.T) {
		specs, err := ParseAggregates(ctx, "  ", allowed)
		if err != nil {
			t.Fatalf("ParseAggregates empty: %v", err)
		}
		if specs != nil {
			t.Fatalf("specs = %v, want nil for empty raw", specs)
		}
		out, err := Aggregate(ctx, db.Model(&aggItem{}), specs)
		if err != nil {
			t.Fatalf("Aggregate(nil specs): %v", err)
		}
		if out != nil {
			t.Fatalf("Aggregate(nil) = %v, want nil", out)
		}
	})

	t.Run("SumAvgMinMaxOverWholeSet", func(t *testing.T) {
		specs, err := ParseAggregates(ctx, "sum:amount,avg:amount,min:amount,max:amount", allowed)
		if err != nil {
			t.Fatalf("ParseAggregates: %v", err)
		}
		out, err := Aggregate(ctx, db.Model(&aggItem{}), specs)
		if err != nil {
			t.Fatalf("Aggregate: %v", err)
		}
		// Rows: 10, 20, 30, 40 -> sum 100, avg 25, min 10, max 40.
		wantString(t, out, "sum:amount", "100")
		wantString(t, out, "avg:amount", "25")
		wantString(t, out, "min:amount", "10")
		wantString(t, out, "max:amount", "40")
	})

	t.Run("RespectsFilteredQuery", func(t *testing.T) {
		specs, err := ParseAggregates(ctx, "sum:amount", allowed)
		if err != nil {
			t.Fatalf("ParseAggregates: %v", err)
		}
		// author_id=1 -> rows 10 + 20 = 30, not the whole-table 100.
		q := db.Model(&aggItem{}).Where("author_id = ?", 1)
		out, err := Aggregate(ctx, q, specs)
		if err != nil {
			t.Fatalf("Aggregate: %v", err)
		}
		wantString(t, out, "sum:amount", "30")
	})

	t.Run("EmptySetIsZero", func(t *testing.T) {
		specs, err := ParseAggregates(ctx, "sum:amount,max:amount", allowed)
		if err != nil {
			t.Fatalf("ParseAggregates: %v", err)
		}
		// A filter that matches nothing: SQL SUM/MAX return NULL, normalized to 0.
		q := db.Model(&aggItem{}).Where("author_id = ?", 999)
		out, err := Aggregate(ctx, q, specs)
		if err != nil {
			t.Fatalf("Aggregate: %v", err)
		}
		wantString(t, out, "sum:amount", "0")
		wantString(t, out, "max:amount", "0")
	})

	t.Run("RejectsUnknownFuncOrField", func(t *testing.T) {
		if _, err := ParseAggregates(ctx, "median:amount", allowed); err == nil {
			t.Fatal("median: want validation error")
		} else {
			assertValidationField(t, err, "aggregate")
		}
		if _, err := ParseAggregates(ctx, "sum:secret", allowed); err == nil {
			t.Fatal("undeclared field: want validation error")
		} else {
			assertValidationField(t, err, "aggregate")
		}
		if _, err := ParseAggregates(ctx, "notapair", allowed); err == nil {
			t.Fatal("malformed pair: want validation error")
		} else {
			assertValidationField(t, err, "aggregate")
		}
	})

	t.Run("DedupesRepeatedPairs", func(t *testing.T) {
		specs, err := ParseAggregates(ctx, "sum:amount,sum:amount", allowed)
		if err != nil {
			t.Fatalf("ParseAggregates: %v", err)
		}
		if len(specs) != 1 {
			t.Fatalf("len(specs) = %d, want 1 (deduped)", len(specs))
		}
	})

	// DecimalPrecision pins the per-driver contract the docs promise (rather than
	// only the evenly-dividing integer cases above): a fractional decimal SUM and
	// a non-integer AVG over a decimal(19,4) column.
	//
	//   - decimal SUM is exact on Postgres/MySQL (fixed point), so those drivers
	//     assert the exact string "30.31"; SQLite computes it in float, so it
	//     asserts the value within tolerance. Asserting the string on SQLite, or
	//     only asserting the value on PG/MySQL, would not lock "exact on PG/MySQL"
	//     — a PG path that scanned SUM through float would still pass a value
	//     check.
	//   - AVG is fractional and rounded to each driver's numeric scale, so it is
	//     an approximation on every driver: value within tolerance, everywhere.
	t.Run("DecimalPrecision", func(t *testing.T) {
		seedAggregateMoney(t, db)
		moneyAllowed := map[string]AggregateColumn{"amount": {Column: "amount"}}
		specs, err := ParseAggregates(ctx, "sum:amount,avg:amount", moneyAllowed)
		if err != nil {
			t.Fatalf("ParseAggregates: %v", err)
		}
		out, err := Aggregate(ctx, db.Model(&aggMoney{}), specs)
		if err != nil {
			t.Fatalf("Aggregate: %v", err)
		}
		// 10.10 + 20.20 + 0.01 = 30.31; avg = 10.1033...
		switch db.Driver() {
		case DriverPostgres, DriverMySQL:
			wantString(t, out, "sum:amount", "30.31") // exact fixed-point SUM
		default: // SQLite: float-approximate
			wantApprox(t, out, "sum:amount", 30.31, 1e-6)
		}
		wantApprox(t, out, "avg:amount", 30.31/3, 1e-3)
	})
}

// wantApprox parses an aggregate's decimal string and asserts it is within eps of
// want. Aggregate values are exact on Postgres/MySQL but float-approximate on
// SQLite (and AVG is approximate everywhere), so the string is not identical
// across drivers; the numeric value is.
func wantApprox(t *testing.T, out map[string]types.Decimal, key string, want, eps float64) {
	t.Helper()
	v, ok := out[key]
	if !ok {
		t.Fatalf("aggregate %q missing from %v", key, out)
	}
	got, _ := v.Float64()
	if diff := got - want; diff < -eps || diff > eps {
		t.Fatalf("aggregate %q = %s (%.10f), want ~%.10f (±%g)", key, v.String(), got, want, eps)
	}
}

func seedAggregateMoney(t *testing.T, db *DB) {
	t.Helper()
	if err := db.AutoMigrate(&aggMoney{}); err != nil {
		t.Fatalf("AutoMigrate(aggMoney) error = %v", err)
	}
	if err := db.Where("1 = 1").Delete(&aggMoney{}).Error; err != nil {
		t.Fatalf("reset money table: %v", err)
	}
	amounts := []string{"10.10", "20.20", "0.01"}
	rows := make([]aggMoney, len(amounts))
	for i, s := range amounts {
		d, err := types.NewDecimalFromString(s)
		if err != nil {
			t.Fatalf("decimal %q: %v", s, err)
		}
		rows[i] = aggMoney{Amount: d}
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed money: %v", err)
	}
}

func wantString(t *testing.T, out map[string]types.Decimal, key, want string) {
	t.Helper()
	v, ok := out[key]
	if !ok {
		t.Fatalf("aggregate %q missing from %v", key, out)
	}
	if got := v.String(); got != want {
		t.Fatalf("aggregate %q = %q, want %q", key, got, want)
	}
}

func seedAggregate(t *testing.T, db *DB) {
	t.Helper()
	if err := db.AutoMigrate(&aggItem{}); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	if err := db.Where("1 = 1").Delete(&aggItem{}).Error; err != nil {
		t.Fatalf("reset table: %v", err)
	}
	rows := []aggItem{
		{AuthorID: 1, Amount: 10},
		{AuthorID: 1, Amount: 20},
		{AuthorID: 2, Amount: 30},
		{AuthorID: 2, Amount: 40},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// --- Runtime contract test -------------------------------------------------
// TestGeneratedAggregateContractRuntime mirrors the exact list input, ListMeta
// meta type, and aggregate wiring the resource generator emits for a resource
// declaring aggregatable fields, and drives it through Huma end to end. It
// proves the generated shape registers with Huma and that aggregates respect
// the list's filters and land in meta.aggregates as exact strings.

type acItem struct {
	ID       uint `gorm:"primaryKey"`
	AuthorID uint
	Amount   int
}

type acListInput struct {
	Page      int    `query:"page" doc:"1-based page"`
	PerPage   int    `query:"per_page" doc:"Page size"`
	Aggregate string `query:"aggregate" doc:"Aggregates"`
	AuthorID  string `query:"author_id" doc:"Filter by AuthorID"`
}

type acListOutput struct {
	Body contract.DataMeta[[]acItem, contract.ListMeta]
}

func TestGeneratedAggregateContractRuntime(t *testing.T) {
	db := openSQLite(t)
	if err := db.AutoMigrate(&acItem{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Where("1 = 1").Delete(&acItem{}).Error; err != nil {
		t.Fatalf("reset: %v", err)
	}
	rows := []acItem{
		{AuthorID: 1, Amount: 10},
		{AuthorID: 1, Amount: 20},
		{AuthorID: 2, Amount: 30},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	list := func(ctx context.Context, in *acListInput) (*acListOutput, error) {
		page, perPage := contract.ClampPage(in.Page, in.PerPage)
		q := db.WithContext(ctx).Model(&acItem{})
		q, err := FilterEq(ctx, q, "author_id", FilterUint, in.AuthorID)
		if err != nil {
			return nil, err
		}
		var total int64
		if err := q.Session(&gorm.Session{}).Count(&total).Error; err != nil {
			return nil, contract.WithContext(ctx, contract.Internal("count"))
		}
		aggs, err := ParseAggregates(ctx, in.Aggregate, map[string]AggregateColumn{"amount": {Column: "amount"}})
		if err != nil {
			return nil, err
		}
		aggregates, err := Aggregate(ctx, q.Session(&gorm.Session{}), aggs)
		if err != nil {
			return nil, err
		}
		var out []acItem
		if err := q.Order("id").Offset(contract.PageOffset(page, perPage)).Limit(perPage).Find(&out).Error; err != nil {
			return nil, contract.WithContext(ctx, contract.Internal("list"))
		}
		return &acListOutput{Body: contract.DataMeta[[]acItem, contract.ListMeta]{
			Data: out,
			Meta: &contract.ListMeta{Page: page, PerPage: perPage, Total: total, Aggregates: aggregates},
		}}, nil
	}

	_, api := humatest.New(t)
	huma.Register(api, huma.Operation{OperationID: "list-ac", Method: http.MethodGet, Path: "/items"}, list)

	// No ?aggregate= -> aggregates omitted from meta.
	if got := decodeAgg(t, api.Get("/items")); got.Meta["aggregates"] != nil {
		t.Fatalf("no aggregate requested but meta.aggregates=%v", got.Meta["aggregates"])
	}
	// Whole set: sum=60, avg=20.
	got := decodeAgg(t, api.Get("/items?aggregate=sum:amount,avg:amount"))
	if got.agg(t, "sum:amount") != "60" || got.agg(t, "avg:amount") != "20" {
		t.Fatalf("whole-set aggregates = %v", got.Meta["aggregates"])
	}
	// Aggregates respect the filter: author_id=1 -> sum=30.
	got = decodeAgg(t, api.Get("/items?author_id=1&aggregate=sum:amount"))
	if got.agg(t, "sum:amount") != "30" {
		t.Fatalf("filtered sum = %q, want 30", got.agg(t, "sum:amount"))
	}
	// Pagination must not shrink the aggregate: one row per page, but sum:amount is
	// still computed over all three matching rows (60), not the single page row.
	got = decodeAgg(t, api.Get("/items?page=1&per_page=1&aggregate=sum:amount"))
	if got.agg(t, "sum:amount") != "60" || len(got.Data) != 1 {
		t.Fatalf("paginated aggregate = %v, data len = %d; want sum 60 over 1 page row", got.Meta["aggregates"], len(got.Data))
	}
	// Bad aggregate spec -> 422.
	if resp := api.Get("/items?aggregate=median:amount"); resp.Code != 422 {
		t.Fatalf("median code = %d, want 422; body=%s", resp.Code, resp.Body.String())
	}
}

type aggDecoded struct {
	Data []acItem       `json:"data"`
	Meta map[string]any `json:"meta"`
}

func (d aggDecoded) agg(t *testing.T, key string) string {
	t.Helper()
	m, ok := d.Meta["aggregates"].(map[string]any)
	if !ok {
		t.Fatalf("meta.aggregates missing or wrong type: %v", d.Meta["aggregates"])
	}
	s, ok := m[key].(string)
	if !ok {
		t.Fatalf("aggregate %q missing or not a string: %v", key, m)
	}
	return s
}

func decodeAgg(t *testing.T, resp *httptest.ResponseRecorder) aggDecoded {
	t.Helper()
	if resp.Code != 200 {
		t.Fatalf("code = %d, want 200; body=%s", resp.Code, resp.Body.String())
	}
	var out aggDecoded
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", resp.Body.String(), err)
	}
	return out
}
