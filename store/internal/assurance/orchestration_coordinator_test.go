package assurance

import (
	"testing"
	"time"

	assertion "github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func TestCoordinatorHistoryRejectsInvalidSuccessors(t *testing.T) {
	now := time.Now().UTC()
	prior := orchestration.CoordinatorLease{SchemaVersion: 1, TeamID: id.NewOrchestrationID(), Scope: cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "history"}}}, Owner: id.New(orchestration.PrefixCoordinator), Generation: 2, Revision: 1, AcquiredAt: now, ExpiresAt: now.Add(time.Minute)}
	_, digest, err := prior.Freeze()
	assertion.NoError(t, err)
	renewal := prior
	renewal.Revision++
	renewal.PreviousDigest = digest
	renewal.ExpiresAt = renewal.ExpiresAt.Add(time.Second)
	assertion.True(t, coordinatorSuccessor(prior, renewal))
	for name, mutate := range map[string]func(*orchestration.CoordinatorLease){
		"wrong predecessor":      func(l *orchestration.CoordinatorLease) { l.PreviousDigest = evidence.Digest(nil) },
		"skipped revision":       func(l *orchestration.CoordinatorLease) { l.Revision++ },
		"different team":         func(l *orchestration.CoordinatorLease) { l.TeamID = id.NewOrchestrationID() },
		"different owner":        func(l *orchestration.CoordinatorLease) { l.Owner = id.New(orchestration.PrefixCoordinator) },
		"shorter lease":          func(l *orchestration.CoordinatorLease) { l.ExpiresAt = prior.ExpiresAt.Add(-time.Second) },
		"changed acquisition":    func(l *orchestration.CoordinatorLease) { l.AcquiredAt = l.AcquiredAt.Add(time.Second) },
		"release extends expiry": func(l *orchestration.CoordinatorLease) { l.Released = true },
	} {
		t.Run(name, func(t *testing.T) {
			next := renewal
			mutate(&next)
			assertion.False(t, coordinatorSuccessor(prior, next))
		})
	}
	takeover := renewal
	takeover.Owner = id.New(orchestration.PrefixCoordinator)
	takeover.Generation++
	takeover.AcquiredAt = prior.ExpiresAt
	takeover.ExpiresAt = takeover.AcquiredAt.Add(time.Minute)
	assertion.True(t, coordinatorSuccessor(prior, takeover))
	takeover.AcquiredAt = prior.ExpiresAt.Add(-time.Nanosecond)
	assertion.False(t, coordinatorSuccessor(prior, takeover))
}

func TestCoordinatorRecordRejectsCorruption(t *testing.T) {
	now := time.Now().UTC()
	lease := orchestration.CoordinatorLease{SchemaVersion: 1, TeamID: id.NewOrchestrationID(), Scope: cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "history"}}}, Owner: id.New(orchestration.PrefixCoordinator), Generation: 2, Revision: 1, AcquiredAt: now, ExpiresAt: now.Add(time.Minute)}
	frozen, digest, err := lease.Freeze()
	assertion.NoError(t, err)
	original, err := record(kindOrchestrationCoordinator, coordinatorKey(lease.TeamID, lease.Revision), digest, lease.TeamID.String(), lease.Scope, frozen)
	assertion.NoError(t, err)
	_, err = decodeCoordinator(original)
	assertion.NoError(t, err)
	for name, mutate := range map[string]func(*Record){
		"deleted": func(r *Record) { r.Deleted = true },
		"digest":  func(r *Record) { r.Digest = evidence.Digest(nil) },
		"body":    func(r *Record) { r.Body = "{}" },
		"id":      func(r *Record) { r.ID = "wrong" },
		"team":    func(r *Record) { r.RunID = id.NewOrchestrationID().String() },
		"kind":    func(r *Record) { r.Kind = "wrong" },
		"scope":   func(r *Record) { r.ScopeCanon = "wrong" },
	} {
		t.Run(name, func(t *testing.T) {
			row := original
			mutate(&row)
			_, readErr := decodeCoordinator(row)
			assertion.ErrorIs(t, readErr, cortex.ErrInvalidContinuation)
		})
	}
}
