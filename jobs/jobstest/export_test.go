package jobstest

// SetMaxRuns lowers RunAll's runaway bound for tests.
func SetMaxRuns(n int) (restore func()) {
	prev := maxRuns
	maxRuns = n
	return func() { maxRuns = prev }
}
