package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/internal/faulttest"
)

// TestFault_Concurrency_RotationLeaderFails: several callers rotate the same
// refresh token at once while the rotating transaction stalls and then
// fails (the replacement token's insert). auth.Service shares one rotation
// among concurrent callers of the same token; callers arriving after it
// ended start their own, which fail the same way (which of the two a
// follower takes is scheduling; the outcome is the same). Expected outcome: a
// terminal error for everyone and no corruption. Every caller returns (none
// is wedged behind the failed leader), every caller sees the failure, the
// token is not revoked or replaced, and once the fault clears it rotates.
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
		results := make(chan error, followers+1)
		rotate := func() {
			_, err := f.svc.RotateRefresh(context.Background(), f.refresh)
			results <- err
		}
		go rotate()
		<-insert.Reached(1) // the leader is inside its transaction, stalled
		started := make(chan struct{}, followers)
		for i := 0; i < followers; i++ {
			go func() {
				started <- struct{}{}
				rotate()
			}()
		}
		for i := 0; i < followers; i++ {
			<-started // each follower is calling in while the leader is stalled
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
		insert.Disarm() // the fault clears
		f.assertUnrotated(t)
	})
}
