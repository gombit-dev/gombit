package migrations

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gombit-dev/gombit/config"
	"gorm.io/gorm"
)

const revisionsTable = "framework_migrations"
const atlasRevisionsTable = "atlas_schema_revisions"
const downSubdir = "downs"

var migrationFilePattern = regexp.MustCompile(`^(\d+)_([A-Za-z0-9][A-Za-z0-9_-]*)\.sql$`)
var migrationDownFilePattern = regexp.MustCompile(`^(\d+)_([A-Za-z0-9][A-Za-z0-9_-]*)\.down\.sql$`)

// Revision is one applied migration recorded in framework_migrations (D4).
type Revision struct {
	Version   string    `gorm:"column:version;primaryKey;size:64"`
	Name      string    `gorm:"column:name;size:255;not null"`
	Batch     int       `gorm:"column:batch;not null"`
	AppliedAt time.Time `gorm:"column:applied_at;not null"`
}

// TableName returns the D4 revision table name.
func (Revision) TableName() string {
	return revisionsTable
}

// MigrationFile is one Atlas versioned migration on disk.
type MigrationFile struct {
	Version  string
	Name     string
	UpPath   string
	DownPath string
}

// ParseMigrationFilename parses an Atlas up migration filename.
func ParseMigrationFilename(filename string) (version string, name string, err error) {
	base := filepath.Base(filename)
	matches := migrationFilePattern.FindStringSubmatch(base)
	if matches == nil {
		return "", "", fmt.Errorf("migrations: invalid migration filename %q", base)
	}
	return matches[1], matches[2], nil
}

// ParseDownFilename parses a Gombit companion down migration filename.
func ParseDownFilename(filename string) (version string, name string, err error) {
	base := filepath.Base(filename)
	matches := migrationDownFilePattern.FindStringSubmatch(base)
	if matches == nil {
		return "", "", fmt.Errorf("migrations: invalid down migration filename %q", base)
	}
	return matches[1], matches[2], nil
}

// DownFilename returns the companion down filename (basename) for an up migration file.
func DownFilename(upFilename string) (string, error) {
	version, name, err := ParseMigrationFilename(upFilename)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s_%s.down.sql", version, name), nil
}

// DownPath returns the path to the Gombit-owned down SQL for an up migration.
// Downs live under <migrationDir>/downs/ so Atlas never scans them (Atlas panics
// on golang-migrate-style .down.sql files in the versioned migration directory).
func DownPath(migrationDir string, version string, name string) string {
	return filepath.Join(migrationDir, downSubdir, fmt.Sprintf("%s_%s.down.sql", version, name))
}

// ListMigrationFiles lists Atlas up migrations in dir, with optional down paths.
// Non-matching *.sql filenames (other than atlas.sum) are skipped with a
// warning. Companion downs live under downs/ and are not listed as up files.
func ListMigrationFiles(dir string) ([]MigrationFile, error) {
	files, _, err := listMigrationFiles(dir)
	return files, err
}

// ListMigrationFilesWithSkipped lists migrations and returns skipped *.sql names.
func ListMigrationFilesWithSkipped(dir string) ([]MigrationFile, []string, error) {
	return listMigrationFiles(dir)
}

func listMigrationFiles(dir string) ([]MigrationFile, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("migrations: read dir: %w", err)
	}

	files := make([]MigrationFile, 0, len(entries))
	skipped := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "atlas.sum" {
			continue
		}
		if strings.HasSuffix(name, ".down.sql") {
			// Misplaced companion downs in the Atlas dir panic migrate apply; warn and skip.
			skipped = append(skipped, name)
			continue
		}
		version, migName, err := ParseMigrationFilename(name)
		if err != nil {
			if strings.HasSuffix(name, ".sql") {
				skipped = append(skipped, name)
			}
			continue
		}
		downPath := DownPath(dir, version, migName)
		if _, statErr := os.Stat(downPath); errors.Is(statErr, os.ErrNotExist) {
			downPath = ""
		} else if statErr != nil {
			return nil, nil, fmt.Errorf("migrations: stat down file: %w", statErr)
		}
		files = append(files, MigrationFile{
			Version:  version,
			Name:     migName,
			UpPath:   filepath.Join(dir, name),
			DownPath: downPath,
		})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Version == files[j].Version {
			return files[i].Name < files[j].Name
		}
		return files[i].Version < files[j].Version
	})
	return files, skipped, nil
}

func warnSkippedMigrationFiles(stderr io.Writer, skipped []string) {
	if stderr == nil || len(skipped) == 0 {
		return
	}
	for _, name := range skipped {
		_, _ = fmt.Fprintf(stderr, "migrations: warning: skipping unrecognized SQL file %q\n", name)
	}
}

func ensureRevisionsTable(db *gorm.DB) error {
	if db == nil {
		return errors.New("migrations: nil db")
	}
	if err := db.AutoMigrate(&Revision{}); err != nil {
		return fmt.Errorf("migrations: ensure %s: %w", revisionsTable, err)
	}
	return nil
}

func listRevisions(db *gorm.DB) ([]Revision, error) {
	var revisions []Revision
	if err := db.Order("version asc").Find(&revisions).Error; err != nil {
		return nil, fmt.Errorf("migrations: list revisions: %w", err)
	}
	return revisions, nil
}

func appliedVersionSet(revisions []Revision) map[string]Revision {
	out := make(map[string]Revision, len(revisions))
	for _, rev := range revisions {
		out[rev.Version] = rev
	}
	return out
}

func nextBatch(db *gorm.DB) (int, error) {
	var maxBatch *int
	if err := db.Model(&Revision{}).Select("MAX(batch)").Scan(&maxBatch).Error; err != nil {
		return 0, fmt.Errorf("migrations: max batch: %w", err)
	}
	if maxBatch == nil {
		return 1, nil
	}
	return *maxBatch + 1, nil
}

func insertBatch(db *gorm.DB, batch int, files []MigrationFile, appliedAt time.Time) error {
	if len(files) == 0 {
		return nil
	}
	revisions := make([]Revision, 0, len(files))
	for _, file := range files {
		revisions = append(revisions, Revision{
			Version:   file.Version,
			Name:      file.Name,
			Batch:     batch,
			AppliedAt: appliedAt,
		})
	}
	if err := db.Create(&revisions).Error; err != nil {
		return fmt.Errorf("migrations: insert batch %d: %w", batch, err)
	}
	return nil
}

func lastBatch(db *gorm.DB) (int, []Revision, error) {
	var maxBatch *int
	if err := db.Model(&Revision{}).Select("MAX(batch)").Scan(&maxBatch).Error; err != nil {
		return 0, nil, fmt.Errorf("migrations: max batch: %w", err)
	}
	if maxBatch == nil {
		return 0, nil, nil
	}
	var revisions []Revision
	if err := db.Where("batch = ?", *maxBatch).Order("version desc").Find(&revisions).Error; err != nil {
		return 0, nil, fmt.Errorf("migrations: list batch %d: %w", *maxBatch, err)
	}
	return *maxBatch, revisions, nil
}

func deleteRevisions(db *gorm.DB, versions []string) error {
	if len(versions) == 0 {
		return nil
	}
	if err := db.Where("version IN ?", versions).Delete(&Revision{}).Error; err != nil {
		return fmt.Errorf("migrations: delete revisions: %w", err)
	}
	return nil
}

// atlasRevisionsSchema resolves the schema both Atlas and Gombit use for
// revision bookkeeping on this database connection. PostgreSQL's current_schema
// is the first usable schema in search_path, which is also where unqualified
// framework tables are created.
func atlasRevisionsSchema(db *gorm.DB) (string, error) {
	if !isPostgresDialect(db) {
		return "", nil
	}
	var schema sql.NullString
	if err := db.Raw("SELECT current_schema()").Scan(&schema).Error; err != nil {
		return "", fmt.Errorf("migrations: resolve atlas revisions schema: %w", err)
	}
	if !schema.Valid || schema.String == "" {
		return "", errors.New("migrations: PostgreSQL connection has no current schema")
	}
	return schema.String, nil
}

// atlasRevisionsRelation returns the quoted relation Atlas's revision table is
// read and written under. PostgreSQL uses the schema resolved once from the
// connection; other drivers keep their unqualified table name.
func atlasRevisionsRelation(db *gorm.DB, schema string) string {
	if isPostgresDialect(db) {
		return quoteIdent(schema) + "." + quoteIdent(atlasRevisionsTable)
	}
	return atlasRevisionsTable
}

func isPostgresDialect(db *gorm.DB) bool {
	return isPostgresDriver(db.Name())
}

func isPostgresDriver(driver string) bool {
	return driver == string(config.DatabaseDriverPostgres)
}

// atlasRevisionsExist reports whether Atlas's revision table exists at the
// location atlasRevisionsRelation names.
func atlasRevisionsExist(db *gorm.DB, schema string) (bool, error) {
	if !isPostgresDialect(db) {
		return db.Migrator().HasTable(atlasRevisionsTable), nil
	}
	var exists bool
	if err := db.Raw("SELECT to_regclass(?) IS NOT NULL", atlasRevisionsRelation(db, schema)).Scan(&exists).Error; err != nil {
		return false, fmt.Errorf("migrations: check atlas revisions: %w", err)
	}
	return exists, nil
}

func deleteAtlasRevisions(db *gorm.DB, schema string, versions []string) error {
	if len(versions) == 0 {
		return nil
	}
	exists, err := atlasRevisionsExist(db, schema)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := db.Exec(
		fmt.Sprintf("DELETE FROM %s WHERE version IN ?", atlasRevisionsRelation(db, schema)),
		versions,
	).Error; err != nil {
		return fmt.Errorf("migrations: delete atlas revisions: %w", err)
	}
	return nil
}

func listAtlasVersions(db *gorm.DB, schema string) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	if db == nil {
		return out, nil
	}
	exists, err := atlasRevisionsExist(db, schema)
	if err != nil {
		return nil, err
	}
	if !exists {
		return out, nil
	}
	var versions []string
	if err := db.Raw(fmt.Sprintf("SELECT version FROM %s", atlasRevisionsRelation(db, schema))).Scan(&versions).Error; err != nil {
		return nil, fmt.Errorf("migrations: list atlas revisions: %w", err)
	}
	for _, version := range versions {
		out[version] = struct{}{}
	}
	return out, nil
}

// preparePostgresAtlasHistory migrates known per-schema history from the old
// shared public table when framework_migrations proves which versions were
// applied in this schema. If existing application tables and a shared public
// history remain but this schema has no attributable history, it fails closed
// rather than letting Atlas rerun old migrations against live objects.
func preparePostgresAtlasHistory(db *gorm.DB, schema string, revisions []Revision) error {
	if !isPostgresDialect(db) || schema == "" || schema == "public" {
		return nil
	}

	localVersions, err := listAtlasVersions(db, schema)
	if err != nil {
		return err
	}
	var missing []string
	for _, revision := range revisions {
		if _, ok := localVersions[revision.Version]; !ok {
			missing = append(missing, revision.Version)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	publicVersions, err := listAtlasVersions(db, "public")
	if err != nil {
		return err
	}
	if len(revisions) > 0 {
		for _, version := range missing {
			if _, ok := publicVersions[version]; !ok {
				return fmt.Errorf("migrations: PostgreSQL history for schema %q is incomplete: framework_migrations records version %s, but neither %s nor public.%s does; reconcile this schema's migration history before retrying", schema, version, atlasRevisionsTable, atlasRevisionsTable)
			}
		}
		if err := seedAtlasRevisionsFromPublic(db, schema); err != nil {
			return err
		}
		localVersions, err = listAtlasVersions(db, schema)
		if err != nil {
			return err
		}
		for _, revision := range revisions {
			if _, ok := localVersions[revision.Version]; !ok {
				return fmt.Errorf("migrations: could not restore Atlas history for schema %q version %s from framework_migrations; reconcile this schema's migration history before retrying", schema, revision.Version)
			}
		}
		return nil
	}

	var unownedPublicVersions []string
	for version := range publicVersions {
		if _, ok := localVersions[version]; !ok {
			unownedPublicVersions = append(unownedPublicVersions, version)
		}
	}
	if len(unownedPublicVersions) == 0 {
		return nil
	}
	var hasApplicationTables bool
	if err := db.Raw(`
SELECT EXISTS (
  SELECT 1
  FROM pg_catalog.pg_class AS relation
  JOIN pg_catalog.pg_namespace AS ns ON ns.oid = relation.relnamespace
  WHERE ns.nspname = ?
    AND relation.relkind IN ('r', 'p')
    AND relation.relname NOT IN (?, ?)
)`, schema, revisionsTable, atlasRevisionsTable).Scan(&hasApplicationTables).Error; err != nil {
		return fmt.Errorf("migrations: check legacy PostgreSQL history for schema %q: %w", schema, err)
	}
	if hasApplicationTables {
		return fmt.Errorf("migrations: legacy PostgreSQL revision history is ambiguous for schema %q: public.%s has versions this schema cannot attribute, and the schema has existing tables; refusing to reapply migrations. Verify which migrations actually ran in this schema, repair its %s and %s history, then retry", schema, atlasRevisionsTable, atlasRevisionsTable, revisionsTable)
	}
	return nil
}

func seedAtlasRevisionsFromPublic(db *gorm.DB, schema string) error {
	localRelation := atlasRevisionsRelation(db, schema)
	publicRelation := atlasRevisionsRelation(db, "public")
	localExists, err := atlasRevisionsExist(db, schema)
	if err != nil {
		return err
	}
	if !localExists {
		if err := db.Exec("CREATE TABLE IF NOT EXISTS " + localRelation + " (LIKE " + publicRelation + " INCLUDING ALL)").Error; err != nil {
			return fmt.Errorf("migrations: create schema-local Atlas history for %q: %w", schema, err)
		}
	}
	query := fmt.Sprintf(`
INSERT INTO %s (version, description, type, applied_at)
SELECT atlas.version, atlas.description, atlas.type, atlas.applied_at
FROM %s AS atlas
JOIN %s AS framework USING (version)
ON CONFLICT (version) DO NOTHING`, localRelation, publicRelation, quoteIdent(revisionsTable))
	if err := db.Exec(query).Error; err != nil {
		return fmt.Errorf("migrations: restore schema-local Atlas history for %q from framework_migrations: %w", schema, err)
	}
	return nil
}

func pendingPresentInAtlas(pending []MigrationFile, atlas map[string]struct{}) []MigrationFile {
	out := make([]MigrationFile, 0, len(pending))
	for _, file := range pending {
		if _, ok := atlas[file.Version]; ok {
			out = append(out, file)
		}
	}
	return out
}

func pendingFiles(files []MigrationFile, applied map[string]Revision) []MigrationFile {
	pending := make([]MigrationFile, 0)
	for _, file := range files {
		if _, ok := applied[file.Version]; !ok {
			pending = append(pending, file)
		}
	}
	return pending
}
