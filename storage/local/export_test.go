package local

// SetSyncDir replaces the directory flush for a test.
func SetSyncDir(fn func(dir string) error) (restore func()) {
	prev := syncDirHook
	syncDirHook = fn
	return func() { syncDirHook = prev }
}

// Path is the file a key's object is stored in.
func (s *Store) Path(key string) string { return s.path(key) }

// UseLinkPublish makes IfAbsent publish with the hard-link fallback, whose
// removal of the temporary name is remove, for a test.
func UseLinkPublish(remove func(string) error) (restore func()) {
	prevPublish, prevRemove := renameNoReplaceHook, removeTempHook
	renameNoReplaceHook, removeTempHook = linkNoReplace, remove
	return func() { renameNoReplaceHook, removeTempHook = prevPublish, prevRemove }
}
