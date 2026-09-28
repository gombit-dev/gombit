package auth

// SetRotateJoinHook makes fn run whenever a caller joins another caller's
// in-flight refresh rotation (before it waits), so a test can pin callers
// inside the shared rotation. Set it before the service is used.
func SetRotateJoinHook(s *Service, fn func()) { s.onRotateJoin = fn }
