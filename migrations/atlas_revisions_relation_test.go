package migrations

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// TestAtlasRevisionsRelationPostgresIsPublicSchema guards issue #482: on
// PostgreSQL, Atlas's revision table is pinned to the public schema by
// withAtlasRevisionsSchema, so Gombit's reads and writes must name public
// explicitly rather than follow the connection's search_path (for example
// tenant1), where the unqualified name would not exist.
//
// No server is needed: a recording database/sql driver answers the queries, so
// the statements the existence probe, list and delete paths actually send are
// the ones asserted.
func TestAtlasRevisionsRelationPostgresIsPublicSchema(t *testing.T) {
	recorder := &queryRecorder{}
	sqlDB := openRecordingDB(t, recorder)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open postgres dialector: %v", err)
	}

	versions, err := listAtlasVersions(db)
	if err != nil {
		t.Fatalf("listAtlasVersions: %v", err)
	}
	if _, ok := versions["20260101000000"]; !ok {
		t.Fatalf("listAtlasVersions() = %v, want version 20260101000000 read back", versions)
	}
	if err := deleteAtlasRevisions(db, []string{"20260101000000"}); err != nil {
		t.Fatalf("deleteAtlasRevisions: %v", err)
	}

	qualified := regexp.MustCompile(`"?public"?\."?atlas_schema_revisions"?`)
	sawProbe, sawList, sawDelete := false, false, false
	for _, q := range recorder.snapshot() {
		if strings.Contains(q, "to_regclass") {
			sawProbe = true
		}
		if strings.HasPrefix(q, "SELECT") && strings.Contains(q, "version") && strings.Contains(q, "atlas_schema_revisions") && !strings.Contains(q, "to_regclass") {
			sawList = true
		}
		if strings.HasPrefix(q, "DELETE") {
			sawDelete = true
		}
		if strings.Contains(q, "atlas_schema_revisions") && !qualified.MatchString(q) {
			t.Errorf("statement %q names atlas_schema_revisions without the public schema; want public.atlas_schema_revisions", q)
		}
	}
	if !sawProbe || !sawList || !sawDelete {
		t.Fatalf("expected probe, list and delete statements; got probe=%v list=%v delete=%v in %q",
			sawProbe, sawList, sawDelete, recorder.snapshot())
	}
}

type queryRecorder struct {
	mu      sync.Mutex
	queries []string
}

func (r *queryRecorder) record(q string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, strings.TrimSpace(q))
}

func (r *queryRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.queries...)
}

func openRecordingDB(t *testing.T, recorder *queryRecorder) *sql.DB {
	t.Helper()
	name := "atlas-revisions-recorder-" + t.Name()
	sql.Register(name, &recordingDriver{rec: recorder})
	sqlDB, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open recording db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

type recordingDriver struct{ rec *queryRecorder }

func (d *recordingDriver) Open(string) (driver.Conn, error) { return &recordingConn{rec: d.rec}, nil }

type recordingConn struct{ rec *queryRecorder }

func (c *recordingConn) Prepare(string) (driver.Stmt, error) {
	return nil, driver.ErrSkip
}
func (c *recordingConn) Close() error              { return nil }
func (c *recordingConn) Begin() (driver.Tx, error) { return nil, driver.ErrSkip }

func (c *recordingConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.rec.record(query)
	switch {
	case strings.Contains(query, "to_regclass"):
		return &staticRows{cols: []string{"exists"}, vals: [][]driver.Value{{true}}}, nil
	case strings.Contains(query, "information_schema"):
		return &staticRows{cols: []string{"count"}, vals: [][]driver.Value{{int64(1)}}}, nil
	case strings.Contains(query, "atlas_schema_revisions"):
		return &staticRows{cols: []string{"version"}, vals: [][]driver.Value{{"20260101000000"}}}, nil
	}
	return &staticRows{cols: []string{"x"}}, nil
}

func (c *recordingConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.rec.record(query)
	return driver.RowsAffected(1), nil
}

type staticRows struct {
	cols []string
	vals [][]driver.Value
	i    int
}

func (r *staticRows) Columns() []string { return r.cols }
func (r *staticRows) Close() error      { return nil }
func (r *staticRows) Next(dest []driver.Value) error {
	if r.i >= len(r.vals) {
		return io.EOF
	}
	copy(dest, r.vals[r.i])
	r.i++
	return nil
}
