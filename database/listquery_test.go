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
	"gorm.io/gorm"
)

type lqItem struct {
	ID       uint `gorm:"primaryKey"`
	Title    string
	AuthorID uint
	Views    int
}

// TestListQueryHelpersSQLite exercises FilterEq / Search / SortBy on SQLite.
// The same assertions run against Postgres and MySQL from the integration suite
// (see TestListQueryHelpers{Postgres,MySQL}) so the SQLite+PG+MySQL matrix the
// working agreement requires is covered.
func TestListQueryHelpersSQLite(t *testing.T) {
	assertListQueryHelpers(t, openSQLite(t))
}

func assertListQueryHelpers(t *testing.T, db *DB) {
	t.Helper()
	seedListQuery(t, db)

	t.Run("FilterEq", func(t *testing.T) {
		ctx := context.Background()
		// Empty raw value is a no-op.
		q, err := FilterEq(ctx, db.Model(&lqItem{}), "author_id", FilterUint, "  ")
		if err != nil {
			t.Fatalf("empty filter: %v", err)
		}
		var all []lqItem
		if err := q.Find(&all).Error; err != nil {
			t.Fatalf("empty filter find: %v", err)
		}
		if len(all) != 3 {
			t.Fatalf("empty filter returned %d rows, want 3 (no-op)", len(all))
		}
		// Coerced uint match.
		q, err = FilterEq(ctx, db.Model(&lqItem{}), "author_id", FilterUint, "1")
		if err != nil {
			t.Fatalf("author filter: %v", err)
		}
		var byAuthor []lqItem
		if err := q.Find(&byAuthor).Error; err != nil {
			t.Fatalf("author filter find: %v", err)
		}
		if len(byAuthor) != 2 {
			t.Fatalf("author_id=1 returned %d rows, want 2", len(byAuthor))
		}
		// A value that does not parse as the kind is a 422 on the column.
		_, err = FilterEq(ctx, db.Model(&lqItem{}), "author_id", FilterUint, "not-a-number")
		assertValidationField(t, err, "author_id")
	})

	t.Run("SearchEscapesWildcards", func(t *testing.T) {
		var matches []lqItem
		if err := Search(db.Model(&lqItem{}), []string{"title"}, "report").Find(&matches).Error; err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(matches) != 1 || matches[0].Title != "Alpha report" {
			t.Fatalf("search 'report' = %+v, want the Alpha row", matches)
		}
		// A literal "%" must not act as a wildcard.
		var literal []lqItem
		if err := Search(db.Model(&lqItem{}), []string{"title"}, "50%").Find(&literal).Error; err != nil {
			t.Fatalf("search literal percent: %v", err)
		}
		if len(literal) != 1 || literal[0].Title != "Beta 50% off" {
			t.Fatalf("search '50%%' = %+v, want only the Beta row (wildcard escaped)", literal)
		}
		// Empty term is a no-op.
		var none []lqItem
		if err := Search(db.Model(&lqItem{}), []string{"title"}, "  ").Find(&none).Error; err != nil {
			t.Fatalf("empty search: %v", err)
		}
		if len(none) != 3 {
			t.Fatalf("empty search returned %d rows, want 3 (no-op)", len(none))
		}
	})

	t.Run("Ordering", func(t *testing.T) {
		ctx := context.Background()
		allowed := []string{"views"}
		firstView := func(ordering string) int {
			t.Helper()
			q, err := Ordering(ctx, db.Model(&lqItem{}), ordering, allowed, "id")
			if err != nil {
				t.Fatalf("Ordering(%q) error = %v", ordering, err)
			}
			var rows []lqItem
			if err := q.Find(&rows).Error; err != nil {
				t.Fatalf("find: %v", err)
			}
			return rows[0].Views
		}
		if got := firstView("views"); got != 10 {
			t.Fatalf("ascending 'views' first Views = %d, want 10", got)
		}
		if got := firstView("-views"); got != 30 {
			t.Fatalf("descending '-views' first Views = %d, want 30", got)
		}
		// Fallback ordering when ordering is empty.
		q, err := Ordering(ctx, db.Model(&lqItem{}), "", allowed, "id")
		if err != nil {
			t.Fatalf("fallback Ordering error = %v", err)
		}
		var rows []lqItem
		if err := q.Find(&rows).Error; err != nil {
			t.Fatalf("fallback find: %v", err)
		}
		if rows[0].ID != 1 {
			t.Fatalf("fallback first row ID = %d, want 1 (order by id)", rows[0].ID)
		}
	})

	t.Run("OrderingRejectsUndeclared", func(t *testing.T) {
		ctx := context.Background()
		_, err := Ordering(ctx, db.Model(&lqItem{}), "author_id", []string{"views"}, "id")
		assertValidationField(t, err, "ordering")
		_, err = Ordering(ctx, db.Model(&lqItem{}), "-author_id", []string{"views"}, "id")
		assertValidationField(t, err, "ordering")
	})
}

func seedListQuery(t *testing.T, db *DB) {
	t.Helper()
	if err := db.AutoMigrate(&lqItem{}); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	if err := db.Where("1 = 1").Delete(&lqItem{}).Error; err != nil {
		t.Fatalf("reset table: %v", err)
	}
	rows := []lqItem{
		{Title: "Alpha report", AuthorID: 1, Views: 30},
		{Title: "Beta 50% off", AuthorID: 1, Views: 10},
		{Title: "Gamma memo", AuthorID: 2, Views: 20},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func assertValidationField(t *testing.T, err error, field string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want validation error on %q", field)
	}
	env, ok := err.(*contract.ErrorEnvelope)
	if !ok {
		t.Fatalf("error type = %T, want *contract.ErrorEnvelope", err)
	}
	if env.Body.Code != contract.CodeValidationError {
		t.Fatalf("error code = %q, want %q", env.Body.Code, contract.CodeValidationError)
	}
	if _, ok := env.Body.Fields[field]; !ok {
		t.Fatalf("validation fields = %v, want a %q entry", env.Body.Fields, field)
	}
}

// --- Runtime contract test -------------------------------------------------
// TestGeneratedListContractRuntime mirrors the exact input struct and handler
// the resource generator emits for a resource declaring searchable / sortable /
// filterable fields (plus a belongs_to, filterable by default), and drives it
// through Huma end to end. It proves the generated shape registers with Huma
// (string query params, no unsupported pointers) and that filter+search+sort+
// pagination behave over a real SQLite database.

type ctItem struct {
	ID       uint `gorm:"primaryKey"`
	Title    string
	AuthorID uint
	Done     bool
}

type ctListInput struct {
	Page     int    `query:"page" doc:"1-based page"`
	PerPage  int    `query:"per_page" doc:"Page size"`
	Search   string `query:"search" doc:"Search"`
	Ordering string `query:"ordering" doc:"Order field; - prefix for DESC"`
	Done     string `query:"done" enum:"true,false" doc:"Filter by Done"`
	AuthorID string `query:"author_id" doc:"Filter by AuthorID"`
}

type ctListOutput struct {
	Body contract.DataMeta[[]ctItem, contract.PageMeta]
}

func TestGeneratedListContractRuntime(t *testing.T) {
	db := openSQLite(t)
	if err := db.AutoMigrate(&ctItem{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Where("1 = 1").Delete(&ctItem{}).Error; err != nil {
		t.Fatalf("reset: %v", err)
	}
	rows := []ctItem{
		{Title: "Zulu", AuthorID: 1, Done: true},
		{Title: "Alpha", AuthorID: 1, Done: false},
		{Title: "Mike", AuthorID: 2, Done: true},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	list := func(ctx context.Context, in *ctListInput) (*ctListOutput, error) {
		page, perPage := contract.ClampPage(in.Page, in.PerPage)
		q := db.WithContext(ctx).Model(&ctItem{})
		q, err := FilterEq(ctx, q, "done", FilterBool, in.Done)
		if err != nil {
			return nil, err
		}
		q, err = FilterEq(ctx, q, "author_id", FilterUint, in.AuthorID)
		if err != nil {
			return nil, err
		}
		q = Search(q, []string{"title"}, in.Search)
		var total int64
		if err := q.Session(&gorm.Session{}).Count(&total).Error; err != nil {
			return nil, contract.WithContext(ctx, contract.Internal("count"))
		}
		q, err = Ordering(ctx, q, in.Ordering, []string{"title"}, "id")
		if err != nil {
			return nil, err
		}
		var out []ctItem
		if err := q.Offset(contract.PageOffset(page, perPage)).Limit(perPage).Find(&out).Error; err != nil {
			return nil, contract.WithContext(ctx, contract.Internal("list"))
		}
		return &ctListOutput{Body: contract.DataMeta[[]ctItem, contract.PageMeta]{Data: out, Meta: &contract.PageMeta{Page: page, PerPage: perPage, Total: total}}}, nil
	}

	_, api := humatest.New(t)
	huma.Register(api, huma.Operation{OperationID: "list-ct", Method: http.MethodGet, Path: "/items"}, list)

	// Filter by FK: author_id=1 -> 2 rows.
	if got := decodeList(t, api.Get("/items?author_id=1")); len(got.Meta) == 0 || got.total() != 2 || len(got.Data) != 2 {
		t.Fatalf("author_id=1 total=%d rows=%d", got.total(), len(got.Data))
	}
	// Filter by bool: done=true -> 2 rows.
	if got := decodeList(t, api.Get("/items?done=true")); got.total() != 2 {
		t.Fatalf("done=true total=%d, want 2", got.total())
	}
	// Search: search=al -> Alpha only.
	if got := decodeList(t, api.Get("/items?search=al")); got.total() != 1 || got.Data[0].Title != "Alpha" {
		t.Fatalf("search=al = %+v", got.Data)
	}
	// Order desc by title (-title) -> Zulu first.
	if got := decodeList(t, api.Get("/items?ordering=-title")); got.Data[0].Title != "Zulu" {
		t.Fatalf("ordering=-title first = %q, want Zulu", got.Data[0].Title)
	}
	// Default order (no ?ordering) is by id -> first inserted (Zulu) first.
	if got := decodeList(t, api.Get("/items")); got.Data[0].Title != "Zulu" {
		t.Fatalf("default first = %q, want Zulu (id order)", got.Data[0].Title)
	}
	// Undeclared ordering field -> 422.
	if resp := api.Get("/items?ordering=author_id"); resp.Code != 422 {
		t.Fatalf("ordering=author_id code = %d, want 422; body=%s", resp.Code, resp.Body.String())
	}
	// Bad filter value -> 422.
	if resp := api.Get("/items?author_id=nope"); resp.Code != 422 {
		t.Fatalf("author_id=nope code = %d, want 422; body=%s", resp.Code, resp.Body.String())
	}
}

type ctDecoded struct {
	Data []ctItem       `json:"data"`
	Meta map[string]any `json:"meta"`
}

func (d ctDecoded) total() int {
	if v, ok := d.Meta["total"].(float64); ok {
		return int(v)
	}
	return -1
}

func decodeList(t *testing.T, resp *httptest.ResponseRecorder) ctDecoded {
	t.Helper()
	if resp.Code != 200 {
		t.Fatalf("code = %d, want 200; body=%s", resp.Code, resp.Body.String())
	}
	var out ctDecoded
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", resp.Body.String(), err)
	}
	return out
}
