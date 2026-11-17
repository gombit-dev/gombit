package migrations

import (
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

// atlasRevisionsRelation returns the name Atlas's revision table is read and
// written under. On PostgreSQL, withAtlasRevisionsSchema pins Atlas to the
// public schema, so the name is qualified; an unqualified name would follow the
// connection's search_path (for example tenant1) and miss the table.
func atlasRevisionsRelation(db *gorm.DB) string {
	if isPostgresDialect(db) {
		return "public." + atlasRevisionsTable
	}
	return atlasRevisionsTable
}

func isPostgresDialect(db *gorm.DB) bool {
	return db.Name() == string(config.DatabaseDriverPostgres)
}

// atlasRevisionsExist reports whether Atlas's revision table exists at the
// location atlasRevisionsRelation names.
func atlasRevisionsExist(db *gorm.DB) (bool, error) {
	if !isPostgresDialect(db) {
		return db.Migrator().HasTable(atlasRevisionsTable), nil
	}
	var exists bool
	if err := db.Raw("SELECT to_regclass(?) IS NOT NULL", atlasRevisionsRelation(db)).Scan(&exists).Error; err != nil {
		return false, fmt.Errorf("migrations: check atlas revisions: %w", err)
	}
	return exists, nil
}

func deleteAtlasRevisions(db *gorm.DB, versions []string) error {
	if len(versions) == 0 {
		return nil
	}
	exists, err := atlasRevisionsExist(db)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := db.Exec(
		fmt.Sprintf("DELETE FROM %s WHERE version IN ?", atlasRevisionsRelation(db)),
		versions,
	).Error; err != nil {
		return fmt.Errorf("migrations: delete atlas revisions: %w", err)
	}
	return nil
}

func listAtlasVersions(db *gorm.DB) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	if db == nil {
		return out, nil
	}
	exists, err := atlasRevisionsExist(db)
	if err != nil {
		return nil, err
	}
	if !exists {
		return out, nil
	}
	var versions []string
	if err := db.Table(atlasRevisionsRelation(db)).Select("version").Find(&versions).Error; err != nil {
		return nil, fmt.Errorf("migrations: list atlas revisions: %w", err)
	}
	for _, version := range versions {
		out[version] = struct{}{}
	}
	return out, nil
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
