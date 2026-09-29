package local

// SetFlushRename replaces the post-rename flush for a test.
func SetFlushRename(fn func(dst string) error) (restore func()) {
	prev := flushRenameHook
	flushRenameHook = fn
	return func() { flushRenameHook = prev }
}
