package resourcegen

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/types"
)

// A generated create body bounds time and date fields to the years every
// supported database can store and return (issue #443): year 0 was stored by
// Postgres as 1 BC and broke every read of the row, and refused by MySQL with a
// 500. The DTO's Resolve rejects it with a 422 naming the field, through Huma.
func TestGeneratedCreateBodyBoundsTimesAndDates(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles and runs a temp module against the framework; skipped in -short")
	}
	type Invoice struct {
		ID      uint `gorm:"primaryKey"`
		Number  string
		Due     time.Time
		Paid    *time.Time
		Issued  types.Date
		Shipped *types.Date
		Noted   sql.NullTime
	}
	res, err := buildModelResource(&Invoice{}, "invoicepkg")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	dto := string(mustFormatGo(renderModelDTOs(res)))
	for _, want := range []string{"types.TimeWithin(b.Due)", "types.TimeWithin(*b.Paid)", "types.DateWithin(b.Issued)", "types.DateWithin(*b.Shipped)", "if b.Noted.Valid", "types.TimeWithin(b.Noted.Time)"} {
		if !strings.Contains(dto, want) {
			t.Fatalf("generated DTO lacks %s:\n%s", want, dto)
		}
	}

	dir := t.TempDir()
	pkgDir := filepath.Join(dir, res.Package)
	if err := os.MkdirAll(pkgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "go.mod"),
		"module invoicemod\n\ngo 1.23\n\nrequire github.com/gombit-dev/gombit v0.0.0\n\nreplace github.com/gombit-dev/gombit => "+resourcegenModuleRoot(t)+"\n")
	writeFile(t, filepath.Join(pkgDir, "model.go"), `package invoicepkg

import (
	"database/sql"
	"time"

	"github.com/gombit-dev/gombit/types"
)

type Invoice struct {
	ID      uint `+"`gorm:\"primaryKey\"`"+`
	Number  string
	Due     time.Time
	Paid    *time.Time
	Issued  types.Date
	Shipped *types.Date
	Noted   sql.NullTime
}
`)
	writeFile(t, filepath.Join(pkgDir, "dto.gen.go"), dto)
	writeFile(t, filepath.Join(pkgDir, "run_test.go"), `package invoicepkg

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humagin"
	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/contract"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestRun(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "t.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Invoice{}); err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	contract.Install(contract.InstallOptions{})
	api := humagin.New(router, contract.HumaConfig("invoice", "0.0.0"))
	type input struct {
		Body invoiceCreateBody
	}
	huma.Register(api, huma.Operation{OperationID: "create-invoice", Method: http.MethodPost, Path: "/invoices"},
		func(_ context.Context, in *input) (*struct{}, error) {
			row := invoiceFromCreateBody(in.Body)
			return &struct{}{}, db.Create(&row).Error
		})
	post := func(raw string) (int, string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/invoices", strings.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	// Every DTO property is required; a nil pointer is sent as null.
	ok := `+"`"+`"number":"P-1","due":"2026-10-04T00:00:00Z","issued":"2026-10-04"`+"`"+`
	nulls := `+"`"+`,"paid":null,"shipped":null,"noted":{"Time":"0001-01-01T00:00:00Z","Valid":false}`+"`"+`
	if code, body := post("{" + ok + nulls + "}"); code >= 300 {
		t.Fatalf("in-range body: %d %s", code, body)
	}
	for field, raw := range map[string]string{
		"due":     `+"`"+`{"number":"P-6","due":"0000-01-01T00:00:00Z","issued":"2026-10-04"`+"`"+` + nulls + "}",
		"paid":    "{" + ok + `+"`"+`,"paid":"9999-12-31T23:00:00Z","shipped":null,"noted":{"Time":"0001-01-01T00:00:00Z","Valid":false}}`+"`"+`,
		"issued":  `+"`"+`{"number":"P-7","due":"2026-10-04T00:00:00Z","issued":"0999-12-31"`+"`"+` + nulls + "}",
		"shipped": "{" + ok + `+"`"+`,"paid":null,"shipped":"0000-01-01","noted":{"Time":"0001-01-01T00:00:00Z","Valid":false}}`+"`"+`,
		"noted":   "{" + ok + `+"`"+`,"paid":null,"shipped":null,"noted":{"Time":"0000-01-01T00:00:00Z","Valid":true}}`+"`"+`,
	} {
		code, body := post(raw)
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, field) || !strings.Contains(body, "must be between") {
			t.Errorf("%s out of range: %d %s; want 422 naming it", field, code, body)
		}
	}
	var n int64
	db.Model(&Invoice{}).Count(&n)
	if n != 1 {
		t.Fatalf("stored %d rows, want only the in-range one", n)
	}
}
`)
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s\n--- generated ---\n%s", err, out, dto)
	}
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated range checks failed: %v\n%s\n--- generated ---\n%s", err, out, dto)
	}
}

// The check is decided from the field's type and calls gombit/types through its
// assigned alias, so a resource package named "types" still compiles.
func TestTimeRangeCheckUsesTheTypesAlias(t *testing.T) {
	type Event struct {
		ID uint `gorm:"primaryKey"`
		At time.Time
	}
	res, err := buildModelResource(&Event{}, "types")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	dto := string(mustFormatGo(renderModelDTOs(res)))
	if !strings.Contains(dto, `types1 "github.com/gombit-dev/gombit/types"`) || !strings.Contains(dto, "types1.TimeWithin(b.At)") {
		t.Fatalf("package types must import gombit/types under another alias:\n%s", dto)
	}
}
