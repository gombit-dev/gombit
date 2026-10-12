package upgrade

// SetLoadedHookForTest runs f after RunCodemods' first load, and returns
// the restore.
func SetLoadedHookForTest(f func()) (restore func()) {
	old := loadedHookForTest
	loadedHookForTest = f
	return func() { loadedHookForTest = old }
}
