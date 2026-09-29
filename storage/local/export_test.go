package local

// SetSyncDir replaces the directory flush for a test.
func SetSyncDir(fn func(dir string) error) (restore func()) {
	prev := syncDirHook
	syncDirHook = fn
	return func() { syncDirHook = prev }
}

// Path is the file a key's object is stored in.
func (s *Store) Path(key string) string { return s.path(key) }
