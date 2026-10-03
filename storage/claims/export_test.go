package claims

// SetSweepBatch sets how many keys Sweep reads at a time, for the test's
// duration (cleanup restores it).
func SetSweepBatch(n int) (restore func()) {
	old := sweepBatch
	sweepBatch = n
	return func() { sweepBatch = old }
}
