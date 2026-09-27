package faulttest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
)

// DBFaults are the faults a wrapped database/sql driver injects, one
// Injector per boundary. A nil Injector never fails. Set them before the
// connection is opened; an Injector's own state (counts, Reset) can be used
// at any time.
type DBFaults struct {
	// Connect is hit when the pool opens a new connection.
	Connect *Injector
	// Begin is hit when a transaction starts.
	Begin *Injector
	// Statement is hit by every Exec and Query, inside a transaction or not,
	// before the statement reaches the database: a failed statement has no
	// effect. With Match set, only matching statements are hit (and counted).
	Statement *Injector
	// Match limits Statement to the statements it reports true for (nil:
	// all). Dialects quote identifiers differently, so match on the table
	// name rather than a quoted form.
	Match func(query string) bool
	// Commit is hit before a transaction commits. A failed commit rolls the
	// transaction back and returns the injected error: nothing it did
	// persists, and its connection goes back to the pool clean. database/sql
	// gives Commit and Rollback no context, so a Delay or BlockUntil here
	// ends only by its timer or channel, never by the caller's context.
	Commit *Injector
	// Rollback is hit before a transaction rolls back. The rollback still
	// happens (the connection must not stay inside the transaction); the
	// injected error is what the caller sees. When the real rollback fails
	// too (after either fault), the error also carries it and
	// driver.ErrBadConn, so the pool discards the connection.
	Rollback *Injector
}

func (f *DBFaults) statement(ctx context.Context, query string) error {
	if f.Match != nil && !f.Match(query) {
		return nil
	}
	return f.Statement.Hit(ctx)
}

// sqlDriverNames are the database/sql driver names the GORM dialectors in
// database register and open.
var sqlDriverNames = map[database.Driver]string{
	database.DriverSQLite:   "sqlite3",
	database.DriverPostgres: "pgx",
	database.DriverMySQL:    "mysql",
}

// OpenDB opens kind's database at dsn through a connection wrapped with
// faults, as a *database.DB (database.OpenConn): code under test gets the
// same GORM setup as from database.Open. Every injector in faults is Reset
// once the database is open, so call 1 is the test's own first call, not
// one GORM made while initializing. The pool gets database.Open's defaults
// for the driver (ConfigurePool). Close the DB when done.
func OpenDB(kind database.Driver, dsn string, faults *DBFaults) (*database.DB, error) {
	name, ok := sqlDriverNames[kind]
	if !ok {
		return nil, fmt.Errorf("faulttest: unsupported driver %q", kind)
	}
	if faults == nil {
		faults = &DBFaults{}
	}
	// sql.Open does not connect; it resolves the registered driver.
	probe, err := sql.Open(name, dsn)
	if err != nil {
		return nil, fmt.Errorf("faulttest: open %s: %w", kind, err)
	}
	base := probe.Driver()
	_ = probe.Close()
	var connector driver.Connector
	if dc, ok := base.(driver.DriverContext); ok {
		if connector, err = dc.OpenConnector(dsn); err != nil {
			return nil, fmt.Errorf("faulttest: open %s: %w", kind, err)
		}
	} else {
		connector = dsnConnector{dsn: dsn, driver: base}
	}
	conn := sql.OpenDB(WrapConnector(connector, faults))
	database.ConfigurePool(conn, config.DatabaseConfig{Driver: config.DatabaseDriver(kind)})
	db, err := database.OpenConn(kind, conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	for _, inj := range []*Injector{faults.Connect, faults.Begin, faults.Statement, faults.Commit, faults.Rollback} {
		inj.Reset()
	}
	return db, nil
}

// dsnConnector adapts a driver without DriverContext.
type dsnConnector struct {
	dsn    string
	driver driver.Driver
}

func (c dsnConnector) Connect(context.Context) (driver.Conn, error) { return c.driver.Open(c.dsn) }
func (c dsnConnector) Driver() driver.Driver                        { return c.driver }

// WrapConnector returns a connector whose connections inject faults (see
// DBFaults). Open it with sql.OpenDB.
func WrapConnector(c driver.Connector, faults *DBFaults) driver.Connector {
	if faults == nil {
		faults = &DBFaults{}
	}
	wrapped := &connector{base: c, faults: faults}
	if closer, ok := c.(io.Closer); ok {
		// database/sql closes a connector that is an io.Closer with the DB.
		return struct {
			*connector
			io.Closer
		}{wrapped, closer}
	}
	return wrapped
}

type connector struct {
	base   driver.Connector
	faults *DBFaults
}

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := c.faults.Connect.Hit(ctx); err != nil {
		return nil, err
	}
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return wrapConn(&faultConn{base: conn, faults: c.faults}), nil
}

// wrapConn gives fc exactly the optional interfaces its base connection
// has among those database/sql decides pool behavior by: a Pinger, and a
// SessionResetter and Validator (together they let the pool keep a
// connection after a canceled transaction). Answering them for a driver
// that does not would change how the pool treats its connections even with
// no fault armed.
func wrapConn(fc *faultConn) driver.Conn {
	p, hasPing := fc.base.(driver.Pinger)
	r, hasReset := fc.base.(driver.SessionResetter)
	v, hasValid := fc.base.(driver.Validator)
	switch {
	case hasPing && hasReset && hasValid:
		return struct {
			*faultConn
			driver.Pinger
			driver.SessionResetter
			driver.Validator
		}{fc, p, r, v}
	case hasPing && hasReset:
		return struct {
			*faultConn
			driver.Pinger
			driver.SessionResetter
		}{fc, p, r}
	case hasPing && hasValid:
		return struct {
			*faultConn
			driver.Pinger
			driver.Validator
		}{fc, p, v}
	case hasReset && hasValid:
		return struct {
			*faultConn
			driver.SessionResetter
			driver.Validator
		}{fc, r, v}
	case hasPing:
		return struct {
			*faultConn
			driver.Pinger
		}{fc, p}
	case hasReset:
		return struct {
			*faultConn
			driver.SessionResetter
		}{fc, r}
	case hasValid:
		return struct {
			*faultConn
			driver.Validator
		}{fc, v}
	default:
		return fc
	}
}

func (c *connector) Driver() driver.Driver { return c.base.Driver() }

// faultConn wraps a driver connection. database/sql uses a connection from
// one goroutine at a time, but skipped is guarded anyway.
type faultConn struct {
	base   driver.Conn
	faults *DBFaults

	mu sync.Mutex
	// skipped is the query whose ExecContext/QueryContext the base driver
	// declined (driver.ErrSkip) after the fault was checked: database/sql
	// runs it again as a prepared statement, which must not count twice.
	skipped string
}

var (
	_ driver.Conn               = (*faultConn)(nil)
	_ driver.ConnBeginTx        = (*faultConn)(nil)
	_ driver.ConnPrepareContext = (*faultConn)(nil)
	_ driver.ExecerContext      = (*faultConn)(nil)
	_ driver.QueryerContext     = (*faultConn)(nil)
	_ driver.NamedValueChecker  = (*faultConn)(nil)
)

func (c *faultConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *faultConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	var (
		st  driver.Stmt
		err error
	)
	if p, ok := c.base.(driver.ConnPrepareContext); ok {
		st, err = p.PrepareContext(ctx, query)
	} else {
		st, err = c.base.Prepare(query)
	}
	if err != nil {
		c.checked(query) // no execution will consume a declined statement's mark
		return nil, err
	}
	fs := &faultStmt{base: st, conn: c, query: query}
	if cc, ok := st.(driver.ColumnConverter); ok { //nolint:staticcheck // forwarded as the driver has it
		return struct {
			*faultStmt
			driver.ColumnConverter //nolint:staticcheck // forwarded as the driver has it
		}{fs, cc}, nil
	}
	return fs, nil
}

func (c *faultConn) Close() error { return c.base.Close() }

func (c *faultConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *faultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := c.faults.Begin.Hit(ctx); err != nil {
		return nil, err
	}
	var (
		tx  driver.Tx
		err error
	)
	if b, ok := c.base.(driver.ConnBeginTx); ok {
		tx, err = b.BeginTx(ctx, opts)
	} else {
		if opts.Isolation != 0 || opts.ReadOnly {
			return nil, errors.New("faulttest: driver does not support transaction options")
		}
		tx, err = c.base.Begin() //nolint:staticcheck // the driver has no BeginTx
	}
	if err != nil {
		return nil, err
	}
	return &faultTx{base: tx, faults: c.faults}, nil
}

func (c *faultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.base.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip // database/sql prepares it; faultStmt checks the fault
	}
	if err := c.faults.statement(ctx, query); err != nil {
		return nil, err
	}
	res, err := e.ExecContext(ctx, query, args)
	if errors.Is(err, driver.ErrSkip) {
		c.skip(query)
	}
	return res, err
}

func (c *faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.base.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	if err := c.faults.statement(ctx, query); err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, query, args)
	if errors.Is(err, driver.ErrSkip) {
		c.skip(query)
	}
	return rows, err
}

func (c *faultConn) skip(query string) {
	c.mu.Lock()
	c.skipped = query
	c.mu.Unlock()
}

// checked reports whether query's fault was already checked by an
// ExecContext/QueryContext the driver declined, and clears the mark.
func (c *faultConn) checked(query string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.skipped != "" && c.skipped == query {
		c.skipped = ""
		return true
	}
	return false
}

func (c *faultConn) CheckNamedValue(nv *driver.NamedValue) error {
	if ch, ok := c.base.(driver.NamedValueChecker); ok {
		return ch.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

type faultStmt struct {
	base  driver.Stmt
	conn  *faultConn
	query string
}

var (
	_ driver.StmtExecContext   = (*faultStmt)(nil)
	_ driver.StmtQueryContext  = (*faultStmt)(nil)
	_ driver.NamedValueChecker = (*faultStmt)(nil)
)

func (s *faultStmt) Close() error  { return s.base.Close() }
func (s *faultStmt) NumInput() int { return s.base.NumInput() }

func (s *faultStmt) hit(ctx context.Context) error {
	if s.conn.checked(s.query) {
		return nil
	}
	return s.conn.faults.statement(ctx, s.query)
}

func (s *faultStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), namedValues(args))
}

func (s *faultStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if err := s.hit(ctx); err != nil {
		return nil, err
	}
	if e, ok := s.base.(driver.StmtExecContext); ok {
		return e.ExecContext(ctx, args)
	}
	values, err := plainValues(args)
	if err != nil {
		return nil, err
	}
	return s.base.Exec(values) //nolint:staticcheck // the driver has no ExecContext
}

func (s *faultStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), namedValues(args))
}

func (s *faultStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if err := s.hit(ctx); err != nil {
		return nil, err
	}
	if q, ok := s.base.(driver.StmtQueryContext); ok {
		return q.QueryContext(ctx, args)
	}
	values, err := plainValues(args)
	if err != nil {
		return nil, err
	}
	return s.base.Query(values) //nolint:staticcheck // the driver has no QueryContext
}

func (s *faultStmt) CheckNamedValue(nv *driver.NamedValue) error {
	if ch, ok := s.base.(driver.NamedValueChecker); ok {
		return ch.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

func namedValues(args []driver.Value) []driver.NamedValue {
	named := make([]driver.NamedValue, len(args))
	for i, v := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return named
}

func plainValues(args []driver.NamedValue) ([]driver.Value, error) {
	values := make([]driver.Value, len(args))
	for i, a := range args {
		if a.Name != "" {
			return nil, errors.New("faulttest: driver does not support named parameters")
		}
		values[i] = a.Value
	}
	return values, nil
}

type faultTx struct {
	base   driver.Tx
	faults *DBFaults
}

func (t *faultTx) Commit() error {
	injected := t.faults.Commit.Hit(context.Background())
	if injected == nil {
		return t.base.Commit()
	}
	return withRollback(injected, t.base.Rollback())
}

func (t *faultTx) Rollback() error {
	injected := t.faults.Rollback.Hit(context.Background())
	rbErr := t.base.Rollback()
	if injected == nil {
		return rbErr
	}
	return withRollback(injected, rbErr)
}

// withRollback is the error for an injected fault whose real rollback
// returned rbErr: the fault alone when the rollback succeeded (the
// connection is clean), else the fault, the rollback's error, and
// driver.ErrBadConn, so the pool discards a connection that may still be
// inside the transaction while errors.Is still finds the fault.
func withRollback(injected, rbErr error) error {
	if rbErr == nil {
		return injected
	}
	return errors.Join(injected, rbErr, driver.ErrBadConn)
}
