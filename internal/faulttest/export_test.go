package faulttest

import "time"

// SetIdleTimeout shortens Idle's wait for tests.
func SetIdleTimeout(d time.Duration) (restore func()) {
	prev := idleTimeout
	idleTimeout = d
	return func() { idleTimeout = prev }
}
