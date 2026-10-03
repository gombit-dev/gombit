//go:build integration

package filefield_test

import (
	"context"
	"errors"
	"flag"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/field"
	"github.com/gombit-dev/gombit/storage/claims"
	"github.com/gombit-dev/gombit/storage/filefield"
	"github.com/gombit-dev/gombit/storage/memory"
	"github.com/gombit-dev/gombit/types"
)

var (
	postgresDSN = flag.String("filefield.postgres-dsn", "", "PostgreSQL DSN for the file field integration tests")
	mysqlDSN    = flag.String("filefield.mysql-dsn", "", "MySQL DSN for the file field integration tests")
)

// integrationFile is a model with a required file and an optional image.
type integrationFile struct {
	ID    uint
	File  types.File   `gorm:"size:512;not null;uniqueIndex"`
	Cover *types.Image `gorm:"size:512;uniqueIndex"`
}

func TestPostgres(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -filefield.postgres-dsn")
	}
	db, err := gorm.Open(postgres.Open(*postgresDSN), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	checkDatabase(t, db)
}

func TestMySQL(t *testing.T) {
	if *mysqlDSN == "" {
		t.Skip("set -filefield.mysql-dsn")
	}
	db, err := gorm.Open(mysql.Open(*mysqlDSN), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	checkDatabase(t, db)
}

// checkDatabase: the file column's unique index (512 bytes, within MySQL's
// key length) holds one record per file, several records without an
// optional file, and a record holds its file's claim, which another
// record then cannot take.
func checkDatabase(t *testing.T, db *gorm.DB) {
	t.Helper()
	ctx := context.Background()
	models := append(claims.Models(), &integrationFile{})
	if err := db.Migrator().DropTable(models...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(models...) })
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatalf("migrate a 512-byte unique file column: %v", err)
	}
	store := memory.New()
	cl := claims.New(db, store)
	p, err := filefield.Policy("prefix=files/file/", field.File, "")
	if err != nil {
		t.Fatal(err)
	}
	long := types.File("files/file/0123456789abcdef0123456789abcdef")
	if err := cl.Pending(ctx, long.Key(), "", time.Now()); err != nil {
		t.Fatal(err)
	}
	putFile(t, store, long.Key(), png, "image/png")
	if err := filefield.Accept(ctx, store, cl, long.Key(), p); err != nil {
		t.Fatalf("Accept = %v", err)
	}
	for i := 0; i < 2; i++ { // two records without a cover: NULLs do not collide
		f := types.File("files/file/other" + string(rune('a'+i)))
		if err := db.Create(&integrationFile{File: f}).Error; err != nil {
			t.Fatalf("create without a cover: %v", err)
		}
	}
	create := func() error {
		return cl.CreateWith(ctx, []string{long.Key()}, func(tx *gorm.DB) error {
			return tx.Create(&integrationFile{File: long}).Error
		})
	}
	if err := create(); err != nil {
		t.Fatal(err)
	}
	if err := create(); !errors.Is(err, claims.ErrNotPending) {
		t.Fatalf("a second record taking a held file = %v, want ErrNotPending", err)
	}
	if err := db.Create(&integrationFile{File: long}).Error; err == nil {
		t.Fatal("the unique index let two records hold one file")
	}
}
