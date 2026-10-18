package upgrade

// SetLoadedHookForTest runs f after RunCodemods' first load, and returns
// the restore.
func SetLoadedHookForTest(f func()) (restore func()) {
	old := loadedHookForTest
	loadedHookForTest = f
	return func() { loadedHookForTest = old }
}

// RegisterCodemodForTest registers c, as a codemod this framework ships,
// and returns the restore.
func RegisterCodemodForTest(c Codemod) (restore func()) {
	old, had := codemods[c.ID]
	codemods[c.ID] = c
	return func() {
		if had {
			codemods[c.ID] = old
		} else {
			delete(codemods, c.ID)
		}
	}
}
