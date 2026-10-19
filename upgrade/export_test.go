package upgrade

// FrameworkTableForTest is a framework model's own table, as the catalog
// says ("" for a model this gombit does not have).
func FrameworkTableForTest(model string) string { return frameworkTables[model] }

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
