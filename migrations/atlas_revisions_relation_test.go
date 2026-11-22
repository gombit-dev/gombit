package migrations

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// TestAtlasRevisionsRelationPostgresUsesCurrentSchema guards issue #482. The
// same schema resolved from the connection must qualify Gombit's reads/writes
// and the Atlas CLI flag, rather than being hard-coded to public.
func TestAtlasRevisionsRelationPostgresUsesCurrentSchema(t *testing.T) {
	recorder := &queryRecorder{}
	sqlDB := openRecordingDB(t, recorder)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open postgres dialector: %v", err)
	}

	schema, err := atlasRevisionsSchema(db)
	if err != nil {
		t.Fatalf("atlasRevisionsSchema: %v", err)
	}
	if schema != "tenant1" {
		t.Fatalf("atlasRevisionsSchema() = %q, want tenant1", schema)
	}
	versions, err := listAtlasVersions(db, schema)
	if err != nil {
		t.Fatalf("listAtlasVersions: %v", err)
	}
	if _, ok := versions["20260101000000"]; !ok {
		t.Fatalf("listAtlasVersions() = %v, want version 20260101000000 read back", versions)
	}
	if err := deleteAtlasRevisions(db, schema, []string{"20260101000000"}); err != nil {
		t.Fatalf("deleteAtlasRevisions: %v", err)
	}

	relation := `"tenant1"."atlas_schema_revisions"`
	sawSchema, sawProbe, sawList, sawDelete := false, false, false, false
	for _, q := range recorder.snapshot() {
		if strings.Contains(q.query, "current_schema()") {
			sawSchema = true
		}
		if strings.Contains(q.query, "to_regclass") {
			sawProbe = true
			if len(q.args) != 1 || q.args[0].Value != relation {
				t.Errorf("to_regclass probe args = %v, want [%q]", q.args, relation)
			}
		}
		if strings.HasPrefix(q.query, "SELECT") && strings.Contains(q.query, "version") && strings.Contains(q.query, "atlas_schema_revisions") && !strings.Contains(q.query, "to_regclass") {
			sawList = true
		}
		if strings.HasPrefix(q.query, "DELETE") {
			sawDelete = true
		}
		if strings.Contains(q.query, "atlas_schema_revisions") && !strings.Contains(q.query, relation) && (len(q.args) != 1 || q.args[0].Value != relation) {
			t.Errorf("statement %q does not use the resolved relation %s", q.query, relation)
		}
	}
	if !sawSchema || !sawProbe || !sawList || !sawDelete {
		t.Fatalf("expected schema resolution, probe, list and delete statements; got schema=%v probe=%v list=%v delete=%v in %v",
			sawSchema, sawProbe, sawList, sawDelete, recorder.snapshot())
	}
}

func TestAtlasRevisionsSchemaRejectsNullCurrentSchema(t *testing.T) {
	recorder := &queryRecorder{nullSchema: true}
	sqlDB := openRecordingDB(t, recorder)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open postgres dialector: %v", err)
	}
	if _, err := atlasRevisionsSchema(db); err == nil || !strings.Contains(err.Error(), "no current schema") {
		t.Fatalf("atlasRevisionsSchema() error = %v, want clear null-current-schema error", err)
	}
}

type queryRecorder struct {
	mu         sync.Mutex
	queries    []recordedQuery
	nullSchema bool
}

type recordedQuery struct {
	query string
	args  []driver.NamedValue
}

func (r *queryRecorder) record(q string, args []driver.NamedValue) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, recordedQuery{query: strings.TrimSpace(q), args: append([]driver.NamedValue(nil), args...)})
}

func (r *queryRecorder) snapshot() []recordedQuery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedQuery(nil), r.queries...)
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

func (c *recordingConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.rec.record(query, args)
	switch {
	case strings.Contains(query, "current_schema()"):
		var schema driver.Value = "tenant1"
		if c.rec.nullSchema {
			schema = nil
		}
		return &staticRows{cols: []string{"current_schema"}, vals: [][]driver.Value{{schema}}}, nil
	case strings.Contains(query, "to_regclass"):
		return &staticRows{cols: []string{"exists"}, vals: [][]driver.Value{{true}}}, nil
	case strings.Contains(query, "information_schema"):
		return &staticRows{cols: []string{"count"}, vals: [][]driver.Value{{int64(1)}}}, nil
	case strings.Contains(query, "atlas_schema_revisions"):
		return &staticRows{cols: []string{"version"}, vals: [][]driver.Value{{"20260101000000"}}}, nil
	}
	return &staticRows{cols: []string{"x"}}, nil
}

func (c *recordingConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.rec.record(query, args)
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
