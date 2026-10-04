package resourcegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gombit-dev/gombit/types"
	"gorm.io/gorm"
)

// Invoice is a package-level type so the generated names (invoiceData,
// createInvoiceInput, …) match the temp module's model.
type Invoice struct {
	gorm.Model
	Number string        `gorm:"not null"`
	Amount types.Decimal `gorm:"type:decimal(19,4);not null"`
}

// TestGeneratedCreateRespondsWithStoredDecimal is #440 end to end through a
// generated handler on SQLite opened by database.Open: a decimal inside the
// contract round-trips exactly and the create response is what get returns;
// one SQLite cannot store exactly, or one over the column's scale, is a 422
// and nothing is stored.
func TestGeneratedCreateRespondsWithStoredDecimal(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles and runs a temp module against the framework; skipped in -short")
	}
	res, err := buildModelResource(&Invoice{}, "invoice")
	if err != nil {
		t.Fatalf("buildModelResource: %v", err)
	}
	handler, err := renderModelHandler(res)
	if err != nil {
		t.Fatalf("renderModelHandler: %v", err)
	}
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "invoice")
	if err := os.MkdirAll(pkgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "go.mod"),
		"module invoiceapp\n\ngo 1.23\n\nrequire github.com/gombit-dev/gombit v0.0.0\n\nreplace github.com/gombit-dev/gombit => "+resourcegenModuleRoot(t)+"\n")
	writeFile(t, filepath.Join(pkgDir, "model.go"),
		"package invoice\n\nimport (\n\t\"github.com/gombit-dev/gombit/types\"\n\t\"gorm.io/gorm\"\n)\n\ntype Invoice struct {\n\tgorm.Model\n\tNumber string `gorm:\"not null\"`\n\tAmount types.Decimal `gorm:\"type:decimal(19,4);not null\"`\n}\n")
	writeFile(t, filepath.Join(pkgDir, "dto.gen.go"), string(mustFormatGo(renderModelDTOs(res))))
	writeFile(t, filepath.Join(pkgDir, "handler.gen.go"), string(mustFormatGo(handler)))
	writeFile(t, filepath.Join(pkgDir, "hooks.go"), string(mustFormatGo(renderModelHooks(res))))
	writeFile(t, filepath.Join(pkgDir, "run_test.go"), decimalCreateRunTest)

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated decimal create failed: %v\n%s\n--- handler ---\n%s", err, out, handler)
	}
}

const decimalCreateRunTest = `package invoice

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/types"
)

func TestDecimalCreate(t *testing.T) {
	db, err := database.Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    filepath.Join(t.TempDir(), "t.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.AutoMigrate(&Invoice{}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: db.DB, Hooks: Hooks{}}
	ctx := context.Background()
	create := func(amount string) (*createInvoiceOutput, error) {
		return h.create(ctx, &createInvoiceInput{Body: invoiceCreateBody{Number: "P-1", Amount: types.MustDecimal(amount)}})
	}

	// Inside the contract: exact, and the create response is the stored row.
	for _, amount := range []string{"99999999999.9999", "1.5000", "0.0001"} {
		out, err := create(amount)
		if err != nil {
			t.Fatalf("create %s: %v", amount, err)
		}
		got, err := h.get(ctx, &getInvoiceInput{ID: strconv.FormatUint(uint64(out.Body.Data.ID), 10)})
		if err != nil {
			t.Fatalf("get %s: %v", amount, err)
		}
		if !got.Body.Data.Amount.Equal(types.MustDecimal(amount).Decimal) {
			t.Fatalf("stored %s, want %s exactly", got.Body.Data.Amount, amount)
		}
		if out.Body.Data.Amount.String() != got.Body.Data.Amount.String() {
			t.Fatalf("create responded %s but get returns %s", out.Body.Data.Amount, got.Body.Data.Amount)
		}
	}

	// The response is the stored row even when the database changes it after
	// the insert (here a trigger; elsewhere a default or a normalized value).
	if err := db.Exec("CREATE TRIGGER stamp AFTER INSERT ON invoices BEGIN UPDATE invoices SET number = 'STORED' WHERE id = NEW.id; END").Error; err != nil {
		t.Fatal(err)
	}
	stamped, err := create("2.5")
	if err != nil {
		t.Fatalf("create with trigger: %v", err)
	}
	if stamped.Body.Data.Number != "STORED" {
		t.Fatalf("create responded number %q, want the stored %q", stamped.Body.Data.Number, "STORED")
	}

	var before int64
	db.Model(&Invoice{}).Count(&before)
	// Outside it: a 422, not a silently changed row.
	for _, amount := range []string{"99999999999999.9999", "123456789012345.1234", "1.00005"} {
		_, err := create(amount)
		var env *contract.ErrorEnvelope
		if !errors.As(err, &env) || env.GetStatus() != 422 {
			t.Fatalf("create %s: error = %v, want a 422", amount, err)
		}
	}
	var after int64
	db.Model(&Invoice{}).Count(&after)
	if after != before {
		t.Fatalf("rows = %d after refused creates, want %d", after, before)
	}
}
`
