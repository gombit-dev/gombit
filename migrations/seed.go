package migrations

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
)

const defaultSeedDir = "database/seeds"

// SeedOptions configures gombit db seed.
type SeedOptions struct {
	WorkDir  string
	SeedDir  string
	Database config.DatabaseConfig
	Stdout   io.Writer
	Stderr   io.Writer

	openDB func(config.DatabaseConfig) (*database.DB, error)
}

func withSeedDefaults(opts SeedOptions) SeedOptions {
	if opts.WorkDir == "" {
		opts.WorkDir = "."
	}
	if opts.SeedDir == "" {
		opts.SeedDir = defaultSeedDir
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	if opts.openDB == nil {
		opts.openDB = database.Open
	}
	return opts
}

func resolveSeedDir(opts SeedOptions) (string, error) {
	absWorkDir, err := filepath.Abs(opts.WorkDir)
	if err != nil {
		return "", fmt.Errorf("migrations: resolve work dir: %w", err)
	}
	seedDir := opts.SeedDir
	if !filepath.IsAbs(seedDir) {
		seedDir = filepath.Join(absWorkDir, seedDir)
	}
	return seedDir, nil
}

// Seed executes SQL seed files from the configured seed directory in lexical order.
//
// Missing or empty seed directories succeed with "No seed files.". Non-.sql entries
// and nested subdirectories are skipped with a warning on stderr. Each file may
// contain multiple statements separated by `;`; statements are executed in order
// and execution stops on the first SQL error.
func Seed(ctx context.Context, opts SeedOptions) error {
	if ctx == nil {
		return errors.New("migrations: nil context")
	}
	opts = withSeedDefaults(opts)
	if err := config.ValidateDatabase(opts.Database); err != nil {
		return err
	}

	seedDir, err := resolveSeedDir(opts)
	if err != nil {
		return err
	}
	files, skipped, skippedDirs, err := listSeedFiles(seedDir)
	if err != nil {
		return err
	}
	warnSkippedSeedFiles(opts.Stderr, skipped)
	warnSkippedSeedDirs(opts.Stderr, skippedDirs)
	if len(files) == 0 {
		_, _ = fmt.Fprintln(opts.Stdout, "No seed files.")
		return nil
	}

	db, err := opts.openDB(opts.Database)
	if err != nil {
		return fmt.Errorf("migrations: open database: %w", err)
	}
	defer func() { _ = db.Close() }()

	for _, path := range files {
		// #nosec G304 -- seed paths are resolved from the configured seed directory.
		sqlBytes, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("migrations: read seed %s: %w", filepath.Base(path), err)
		}
		sqlText := strings.TrimSpace(string(sqlBytes))
		if sqlText == "" {
			return fmt.Errorf("migrations: seed %s is empty", filepath.Base(path))
		}
		stmts := splitSQLStatements(sqlText)
		if len(stmts) == 0 {
			return fmt.Errorf("migrations: seed %s has no executable statements", filepath.Base(path))
		}
		for i, stmt := range stmts {
			if err := db.Exec(stmt).Error; err != nil {
				return fmt.Errorf("migrations: execute seed %s statement %d: %w", filepath.Base(path), i+1, err)
			}
		}
	}
	_, _ = fmt.Fprintf(opts.Stdout, "Applied %d seed file(s).\n", len(files))
	return nil
}

func listSeedFiles(dir string) (files []string, skipped []string, skippedDirs []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil, nil
		}
		return nil, nil, nil, fmt.Errorf("migrations: read seed dir: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			skippedDirs = append(skippedDirs, name)
			continue
		}
		if strings.HasSuffix(strings.ToLower(name), ".sql") {
			files = append(files, filepath.Join(dir, name))
			continue
		}
		skipped = append(skipped, name)
	}
	sort.Strings(files)
	return files, skipped, skippedDirs, nil
}

func warnSkippedSeedFiles(stderr io.Writer, skipped []string) {
	if stderr == nil || len(skipped) == 0 {
		return
	}
	for _, name := range skipped {
		_, _ = fmt.Fprintf(stderr, "migrations: warning: skipping non-SQL seed file %s\n", name)
	}
}

func warnSkippedSeedDirs(stderr io.Writer, skippedDirs []string) {
	if stderr == nil || len(skippedDirs) == 0 {
		return
	}
	for _, name := range skippedDirs {
		_, _ = fmt.Fprintf(stderr, "migrations: warning: skipping nested seed directory %s (flat directory only)\n", name)
	}
}

// splitSQLStatements splits SQL on semicolons outside quotes and comments.
// Empty / comment-only fragments are dropped.
//
// v0.1 known limits: MySQL backtick identifiers and PostgreSQL dollar-quoted
// strings are not treated as quoted regions, so a ';' inside them can mis-split.
func splitSQLStatements(sql string) []string {
	var (
		stmts                                             []string
		current                                           strings.Builder
		inSingle, inDouble, inLineComment, inBlockComment bool
		// A CREATE TRIGGER / FUNCTION / PROCEDURE body is a BEGIN ... END
		// block whose inner ';' must not end the statement.
		word               strings.Builder
		wordCount          int
		isCreate, compound bool
		blockDepth         int
		prevWord           string
	)
	// flushWord classifies the word just ended by the rune next. Inside a
	// compound body, END closes the innermost block; END IF / END LOOP /
	// END WHILE / END REPEAT / END CASE name the block END already closed,
	// so the word after END is not an opener. IF / LOOP / WHILE / REPEAT
	// open a block, except IF( / REPEAT( (function calls) and DDL IF NOT EXISTS.
	flushWord := func(next rune) {
		if word.Len() == 0 {
			return
		}
		w := word.String()
		word.Reset()
		wordCount++
		last := prevWord
		prevWord = w
		switch {
		case wordCount == 1:
			isCreate = w == "CREATE"
		case isCreate && wordCount <= 4 && (w == "TRIGGER" || w == "FUNCTION" || w == "PROCEDURE"):
			compound = true
		case !compound:
		case w == "END":
			if blockDepth > 0 {
				blockDepth--
			}
		case last == "END":
		case w == "BEGIN" || w == "CASE":
			blockDepth++
		case w == "IF" && next != '(' && !isDDLBefore(last):
			blockDepth++
		case (w == "LOOP" || w == "WHILE" || w == "REPEAT") && next != '(':
			blockDepth++
		}
	}
	resetStatement := func() {
		wordCount, isCreate, compound, blockDepth, prevWord = 0, false, false, 0, ""
	}

	runes := []rune(sql)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		next := rune(0)
		if i+1 < len(runes) {
			next = runes[i+1]
		}

		inCode := !inLineComment && !inBlockComment && !inSingle && !inDouble
		if inCode && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
			word.WriteRune(unicode.ToUpper(r))
		} else {
			flushWord(r)
		}

		switch {
		case inLineComment:
			current.WriteRune(r)
			if r == '\n' {
				inLineComment = false
			}
		case inBlockComment:
			current.WriteRune(r)
			if r == '*' && next == '/' {
				current.WriteRune(next)
				i++
				inBlockComment = false
			}
		case inSingle:
			current.WriteRune(r)
			if r == '\'' {
				// SQL escaped quote: ''
				if next == '\'' {
					current.WriteRune(next)
					i++
					continue
				}
				inSingle = false
			}
		case inDouble:
			current.WriteRune(r)
			if r == '"' {
				if next == '"' {
					current.WriteRune(next)
					i++
					continue
				}
				inDouble = false
			}
		case r == '-' && next == '-':
			current.WriteRune(r)
			current.WriteRune(next)
			i++
			inLineComment = true
		case r == '/' && next == '*':
			current.WriteRune(r)
			current.WriteRune(next)
			i++
			inBlockComment = true
		case r == '\'':
			current.WriteRune(r)
			inSingle = true
		case r == '"':
			current.WriteRune(r)
			inDouble = true
		case r == ';':
			if compound && blockDepth > 0 {
				current.WriteRune(r)
				continue
			}
			if stmt := strings.TrimSpace(current.String()); stmt != "" && !isSQLCommentOnly(stmt) {
				stmts = append(stmts, stmt)
			}
			current.Reset()
			resetStatement()
		default:
			current.WriteRune(r)
		}
	}
	flushWord(0)
	if stmt := strings.TrimSpace(current.String()); stmt != "" && !isSQLCommentOnly(stmt) {
		stmts = append(stmts, stmt)
	}
	return stmts
}

// isDDLBefore reports whether the word before IF is a DDL keyword, so that
// IF EXISTS / IF NOT EXISTS is not taken for a control-flow IF.
func isDDLBefore(word string) bool {
	switch word {
	case "DROP", "TABLE", "VIEW", "INDEX", "DATABASE", "SCHEMA":
		return true
	}
	return false
}

func isSQLCommentOnly(stmt string) bool {
	remaining := strings.TrimSpace(stmt)
	for remaining != "" {
		if strings.HasPrefix(remaining, "--") {
			if idx := strings.IndexByte(remaining, '\n'); idx >= 0 {
				remaining = strings.TrimSpace(remaining[idx+1:])
				continue
			}
			return true
		}
		if strings.HasPrefix(remaining, "/*") {
			end := strings.Index(remaining, "*/")
			if end < 0 {
				return true
			}
			remaining = strings.TrimSpace(remaining[end+2:])
			continue
		}
		return false
	}
	return true
}
