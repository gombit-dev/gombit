package jobs

import "time"

// SetShutdownGrace shortens the canceled-handler grace for tests.
func SetShutdownGrace(d time.Duration) (restore func()) {
	prev := shutdownGrace
	shutdownGrace = d
	return func() { shutdownGrace = prev }
}

// ReserveBackoff exposes reserveBackoff.
var ReserveBackoff = reserveBackoff
