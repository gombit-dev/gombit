package presign

import "time"

// SetUploadTimeout sets how long after its URL expires a signed PUT may
// still be writing, until restore is called.
func SetUploadTimeout(d time.Duration) (restore func()) {
	old := uploadTimeout
	uploadTimeout = d
	return func() { uploadTimeout = old }
}
