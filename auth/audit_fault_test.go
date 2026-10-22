package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/internal/faulttest"
)

// TestFault_Database_AuditInsertStallIsBounded: the audit insert stalls in
// the driver and never comes back. Recording is best effort, and the
// operation it records has committed: the operation answers once the audit
// write's own deadline passes, not never, even with a context that has no
// deadline of its own.
func TestFault_Database_AuditInsertStallIsBounded(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		auth.SetAuditTimeout(t, 100*time.Millisecond)
		never := make(chan struct{})
		f := newRotationFixture(t, kind, dsn, &faulttest.DBFaults{
			Statement: faulttest.BlockUntil(never),
			Match:     faulttest.Inserts("auth_audit_events"),
		})
		done := make(chan error, 1)
		go func() {
			_, err := f.svc.Register(context.Background(), "stalled-audit@example.com", "correct horse battery")
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Register with a stalled audit insert = %v, want the registration", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Register waited on a stalled audit insert past its deadline")
		}
		var n int64
		if err := f.db.Model(&auth.User{}).Where("email = ?", "stalled-audit@example.com").Count(&n).Error; err != nil || n != 1 {
			t.Fatalf("the committed user: %d rows, %v", n, err)
		}
	})
}
