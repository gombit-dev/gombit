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

// SetPurgeBatch shrinks the Redis purge batch for tests.
func SetPurgeBatch(n int) (restore func()) {
	prev := purgeBatch
	purgeBatch = n
	return func() { purgeBatch = prev }
}

// SetFailedPageSize shrinks the Redis failed-set page for tests.
func SetFailedPageSize(n int) (restore func()) {
	prev := failedPageSize
	failedPageSize = n
	return func() { failedPageSize = prev }
}

// SetFailedPageHook runs fn between the pages of a Redis Failed read.
func SetFailedPageHook(fn func()) (restore func()) {
	prev := failedPageHook
	failedPageHook = fn
	return func() { failedPageHook = prev }
}
