package database

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gombit-dev/gombit/config"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Driver names a supported database driver.
type Driver string

const (
	// DriverSQLite identifies SQLite.
	DriverSQLite Driver = Driver(config.DatabaseDriverSQLite)
	// DriverPostgres identifies PostgreSQL.
	DriverPostgres Driver = Driver(config.DatabaseDriverPostgres)
	// DriverMySQL identifies MySQL.
	DriverMySQL Driver = Driver(config.DatabaseDriverMySQL)
)

// Capabilities names behavior that differs across supported database drivers.
type Capabilities struct {
	Transactions          bool
	Savepoints            bool
	ForeignKeyConstraints bool
	Returning             bool
	Upsert                bool
	AdvisoryLocks         bool
	ConcurrentIndexBuilds bool
}

// DB is an opened GORM database with Gombit driver metadata.
type DB struct {
	*gorm.DB

	driver       Driver
	capabilities Capabilities
}

// Open opens a GORM database for a supported config.DatabaseConfig.
func Open(cfg config.DatabaseConfig) (*DB, error) {
	if err := config.ValidateDatabase(cfg); err != nil {
		return nil, err
	}

	driver := Driver(cfg.Driver)
	dialector, err := dialectorFor(driver, cfg.DSN)
	if err != nil {
		return nil, err
	}

	db, err := open(driver, dialector)
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.SQLDB()
	if err != nil {
		return nil, err
	}
	ConfigurePool(sqlDB, cfg)
	return db, nil
}

// ConfigurePool applies Open's pool settings to conn: cfg's MaxOpenConns,
// MaxIdleConns, and ConnMaxLifetime, and the driver-aware defaults for
// cfg.Driver where they are zero. OpenConn leaves a caller's handle alone;
// call this to give it what Open would have.
func ConfigurePool(conn *sql.DB, cfg config.DatabaseConfig) {
	applyPoolConfig(conn, poolConfigFor(Driver(cfg.Driver), cfg))
}

// OpenConn opens a GORM database for driver over conn, a database/sql
// handle the caller built: one whose driver is wrapped for instrumentation
// or, in tests, for fault injection. The DB behaves as one from Open (the
// same error translation and model validation). The caller owns conn's pool
// settings (ConfigurePool gives it Open's); Close closes conn.
func OpenConn(driver Driver, conn *sql.DB) (*DB, error) {
	if conn == nil {
		return nil, errors.New("database: OpenConn: nil conn")
	}
	var dialector gorm.Dialector
	switch driver {
	case DriverSQLite:
		dialector = sqlite.New(sqlite.Config{Conn: conn})
	case DriverPostgres:
		dialector = postgres.New(postgres.Config{Conn: conn})
	case DriverMySQL:
		dialector = mysql.New(mysql.Config{Conn: conn})
	default:
		return nil, fmt.Errorf("database: unsupported driver %q", driver)
	}
	return open(driver, dialector)
}

func open(driver Driver, dialector gorm.Dialector) (*DB, error) {
	// TranslateError lets each dialector map its own structured error codes
	// (Postgres SQLSTATE, MySQL error numbers, SQLite extended result codes)
	// to portable gorm.ErrXxx sentinels, so database/errors.go can classify
	// unique/foreign-key violations by errors.Is instead of driver-specific
	// message text.
	gormDB, err := gorm.Open(dialector, &gorm.Config{TranslateError: true})
	if err != nil {
		return nil, fmt.Errorf("database: open %s: %w", driver, err)
	}

	// Run model Validate hooks on every create/update, so a domain invariant
	// enforced once is enforced on both the API and admin write paths.
	if err := registerValidationCallback(gormDB); err != nil {
		return nil, err
	}

	return &DB{
		DB:           gormDB,
		driver:       driver,
		capabilities: CapabilitiesFor(driver),
	}, nil
}

// Driver returns the configured driver.
func (db *DB) Driver() Driver {
	if db == nil {
		return ""
	}
	return db.driver
}

// Capabilities returns the configured driver's capability flags.
func (db *DB) Capabilities() Capabilities {
	if db == nil {
		return Capabilities{}
	}
	return db.capabilities
}

// SQLDB returns the underlying database/sql handle.
func (db *DB) SQLDB() (*sql.DB, error) {
	if db == nil || db.DB == nil {
		return nil, errors.New("database: nil db")
	}
	sqlDB, err := db.DB.DB()
	if err != nil {
		return nil, fmt.Errorf("database: sql db: %w", err)
	}
	return sqlDB, nil
}

// Close closes the underlying database/sql handle.
func (db *DB) Close() error {
	sqlDB, err := db.SQLDB()
	if err != nil {
		return err
	}
	if err := sqlDB.Close(); err != nil {
		return fmt.Errorf("database: close: %w", err)
	}
	return nil
}

// CapabilitiesFor returns the capability model for driver.
func CapabilitiesFor(driver Driver) Capabilities {
	switch driver {
	case DriverSQLite:
		return Capabilities{
			Transactions:          true,
			Savepoints:            true,
			ForeignKeyConstraints: true,
			Returning:             true,
			Upsert:                true,
		}
	case DriverPostgres:
		return Capabilities{
			Transactions:          true,
			Savepoints:            true,
			ForeignKeyConstraints: true,
			Returning:             true,
			Upsert:                true,
			AdvisoryLocks:         true,
			ConcurrentIndexBuilds: true,
		}
	case DriverMySQL:
		return Capabilities{
			Transactions:          true,
			Savepoints:            true,
			ForeignKeyConstraints: true,
			Upsert:                true,
		}
	default:
		return Capabilities{}
	}
}

func dialectorFor(driver Driver, dsn string) (gorm.Dialector, error) {
	switch driver {
	case DriverSQLite:
		return sqlite.Open(dsn), nil
	case DriverPostgres:
		return postgres.Open(dsn), nil
	case DriverMySQL:
		return mysql.Open(dsn), nil
	default:
		return nil, fmt.Errorf("database: unsupported driver %q", driver)
	}
}

type poolConfig struct {
	maxOpenConns    int
	maxIdleConns    int
	connMaxLifetime time.Duration
}

func poolConfigFor(driver Driver, cfg config.DatabaseConfig) poolConfig {
	pool := defaultPoolConfig(driver)
	if cfg.MaxOpenConns > 0 {
		pool.maxOpenConns = cfg.MaxOpenConns
	}
	if cfg.MaxIdleConns > 0 {
		pool.maxIdleConns = cfg.MaxIdleConns
	}
	if cfg.ConnMaxLifetime > 0 {
		pool.connMaxLifetime = cfg.ConnMaxLifetime
	}
	return pool
}

func defaultPoolConfig(driver Driver) poolConfig {
	switch driver {
	case DriverSQLite:
		return poolConfig{
			maxOpenConns: 1,
			maxIdleConns: 1,
		}
	case DriverPostgres, DriverMySQL:
		return poolConfig{
			maxOpenConns:    25,
			maxIdleConns:    5,
			connMaxLifetime: 30 * time.Minute,
		}
	default:
		return poolConfig{}
	}
}

func applyPoolConfig(db *sql.DB, cfg poolConfig) {
	if cfg.maxOpenConns > 0 {
		db.SetMaxOpenConns(cfg.maxOpenConns)
	}
	if cfg.maxIdleConns > 0 {
		db.SetMaxIdleConns(cfg.maxIdleConns)
	}
	if cfg.connMaxLifetime > 0 {
		db.SetConnMaxLifetime(cfg.connMaxLifetime)
	}
}
