package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/internal/faulttest"
)

// TestFault_Concurrency_RotationLeaderFails: several callers rotate the same
// refresh token at once while the rotating transaction stalls and then
// fails (the replacement token's insert). auth.Service shares one rotation
// among concurrent callers of the same token: the followers are pinned
// inside that shared rotation (joined, waiting on the leader) before the
// leader is released. Expected outcome: a terminal error for everyone and no
// corruption. Every caller returns (none is wedged behind the failed
// leader), every caller sees the leader's failure, only the leader touched
// the database (one INSERT: nobody ran an independent rotation), the token is
// not revoked or replaced, and once the fault clears it rotates.
func TestFault_Concurrency_RotationLeaderFails(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		release := make(chan struct{})
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		insert := faulttest.SequenceThen(faulttest.Failure(faulttest.ErrInjected),
			faulttest.BlockThenFail(release, faulttest.ErrInjected))
		f := newRotationFixture(t, kind, dsn, &faulttest.DBFaults{Statement: insert, Match: insertsRefreshToken})

		const followers = 4
		joined := make(chan struct{}, followers)
		auth.SetRotateJoinHook(f.svc, func() { joined <- struct{}{} })
		results := make(chan error, followers+1)
		rotate := func() {
			_, err := f.svc.RotateRefresh(context.Background(), f.refresh)
			results <- err
		}
		go rotate()
		<-insert.Reached(1) // the leader is inside its transaction, stalled
		for i := 0; i < followers; i++ {
			go rotate()
		}
		for i := 0; i < followers; i++ {
			select {
			case <-joined: // a follower is waiting on the leader's rotation
			case err := <-results:
				t.Fatalf("a follower returned (%v) without joining the stalled leader's rotation", err)
			case <-time.After(5 * time.Second):
				t.Fatalf("%d of %d followers never joined the in-flight rotation", followers-i, followers)
			}
		}
		close(release) // the leader's insert fails; its transaction rolls back

		for i := 0; i < followers+1; i++ {
			select {
			case err := <-results:
				if !errors.Is(err, faulttest.ErrInjected) {
					t.Fatalf("a caller got %v, want the rotation's failure", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%d of %d callers are wedged behind the failed rotation", followers+1-i, followers+1)
			}
		}
		if n := insert.Calls(); n != 1 {
			t.Fatalf("%d replacement-token inserts, want 1: every follower should have shared the leader's rotation", n)
		}
		insert.Disarm() // the fault clears
		f.assertUnrotated(t)
	})
}
