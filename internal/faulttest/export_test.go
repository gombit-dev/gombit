package faulttest

import (
	"time"

	"github.com/gombit-dev/gombit/database"
)

// SetIdleTimeout shortens Idle's wait for tests.
func SetIdleTimeout(d time.Duration) (restore func()) {
	prev := idleTimeout
	idleTimeout = d
	return func() { idleTimeout = prev }
}

// SetRetryGuard shortens CheckRetryPolicy's runaway guard for tests.
func SetRetryGuard(d time.Duration) (restore func()) {
	prev := retryGuard
	retryGuard = d
	return func() { retryGuard = prev }
}

// AwaitLockWaitErr is AwaitLockWait returning its error, with timeout.
func AwaitLockWaitErr(kind database.Driver, dsn, table string, timeout time.Duration) error {
	return awaitLockWait(kind, dsn, table, timeout)
}
